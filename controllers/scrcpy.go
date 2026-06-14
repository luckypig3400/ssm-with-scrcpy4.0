// Copyright (C) 2024, 2025 kvarenzn
// SPDX-License-Identifier: GPL-3.0-or-later

package controllers

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"

	"github.com/kvarenzn/ssm/adb"
	"github.com/kvarenzn/ssm/common"
	"github.com/kvarenzn/ssm/config"
	"github.com/kvarenzn/ssm/decoders/av"
	"github.com/kvarenzn/ssm/log"
	"github.com/kvarenzn/ssm/stage"
)

type ScrcpyController struct {
	device    *adb.Device
	sessionID string

	listener      net.Listener
	videoSocket   net.Conn
	controlSocket net.Conn

	width    int
	height   int
	codecID  string
	decoder  *av.AVDecoder
	cRunning bool
	vRunning bool
}

func NewScrcpyController(device *adb.Device) *ScrcpyController {
	return &ScrcpyController{
		device:    device,
		sessionID: fmt.Sprintf("%08x", rand.Int31()),
	}
}

func tryListen(host string, port int) (net.Listener, int) {
	for {
		addr := fmt.Sprintf("%s:%d", host, port)
		listen, err := net.Listen("tcp", addr)
		if err == nil {
			return listen, port
		}

		port++
	}
}

const testFromPort = 27188

func (c *ScrcpyController) Open(filepath string, version string) error {
	listener, port := tryListen("localhost", testFromPort)
	c.listener = listener
	log.Debugf("Listening at localhost:%d", port)

	localName := fmt.Sprintf("localabstract:scrcpy_%s", c.sessionID)
	err := c.device.Forward(localName, fmt.Sprintf("tcp:%d", port), true, false)
	if err != nil {
		return err
	}
	log.Debugf("ADB reverse socket `%s` created.", localName)

	f, err := os.Open(filepath)
	if err != nil {
		return err
	}

	log.Debugln("`scrcpy-server` loaded.")

	if err := c.device.Push(f, "/data/local/tmp/scrcpy-server.jar"); err != nil {
		return err
	}

	log.Debugln("`scrcpy-server` pushed to gaming device.")

	go func() {
		result, err := c.device.Sh(
			"CLASSPATH=/data/local/tmp/scrcpy-server.jar",
			"app_process",
			"/",
			"com.genymobile.scrcpy.Server",
			version,
			fmt.Sprintf("scid=%s", c.sessionID), // session id
			"log_level=info",                    // log level
			"audio=false",                       // disable audio sync
			"clipboard_autosync=false",          // disable clipboard
		)
		if err != nil {
			log.Fatalln("Failed to start `scrcpy-server`:", err)
		}

		log.Debugln(result)
	}()

	videoSocket, err := listener.Accept()
	if err != nil {
		return err
	}
	c.videoSocket = videoSocket

	log.Debugln("Video socket accepted.")

	controlSocket, err := listener.Accept()
	if err != nil {
		return err
	}
	c.controlSocket = controlSocket

	log.Debugln("Control socket accepted.")

	err = c.device.Client().KillForward(localName, true)
	if err != nil {
		return err
	}

	log.Debugf("ADB reverse socket `%s` removed.", localName)

	deviceName := make([]byte, 64)
	videoSocket.Read(deviceName)

	buf := make([]byte, 4)
	videoSocket.Read(buf)
	c.codecID = string(buf)

	c.decoder, err = av.NewAVDecoder(c.codecID)
	if err != nil {
		return err
	}

	videoSocket.Read(buf)
	c.width = int(binary.BigEndian.Uint32(buf))

	videoSocket.Read(buf)
	c.height = int(binary.BigEndian.Uint32(buf))

	c.cRunning = true
	c.vRunning = true

	go func() {
		msgTypeBuf := make([]byte, 1)
		sizeBuf := make([]byte, 4)
		for c.cRunning {
			if n, err := controlSocket.Read(msgTypeBuf); err != nil || n != 1 {
				break
			}

			if n, err := controlSocket.Read(sizeBuf); err != nil || n != 4 {
				break
			}

			size := binary.BigEndian.Uint32(sizeBuf)
			bodyBuf := make([]byte, size)
			if n, err := controlSocket.Read(bodyBuf); err != nil || n != int(size) {
				break
			}
		}

		c.cRunning = false
	}()

	go func() {
		ptsBuf := make([]byte, 8)
		sizeBuf := make([]byte, 4)
		for c.vRunning {
			if n, err := videoSocket.Read(ptsBuf); err != nil || n != 8 {
				break
			}

			pts := binary.BigEndian.Uint64(ptsBuf)

			if n, err := videoSocket.Read(sizeBuf); err != nil || n != 4 {
				break
			}

			size := binary.BigEndian.Uint32(sizeBuf)

			data := make([]byte, size)

			if n, err := videoSocket.Read(data); err != nil || n != int(size) {
				break
			}

			c.decoder.Decode(pts, data)
		}

		c.vRunning = false
	}()

	return nil
}

func (c *ScrcpyController) Encode(action common.TouchAction, x, y int32, pointerID uint64) []byte {
	// 新版 scrcpy-server 觸控封包長度為 34 bytes
	b := make([]byte, 34)
	
	// type (1 byte): 2 代表 SC_CONTROL_MSG_TYPE_INJECT_TOUCH_EVENT
	b[0] = 2 
	
	// action (1 byte): 0=Down, 1=Up, 2=Move 等
	b[1] = byte(action)

	// pointer_id (8 bytes)
	binary.BigEndian.PutUint64(b[2:10], pointerID)

	// position.x (4 bytes)
	binary.BigEndian.PutUint32(b[10:14], uint32(x))
	
	// position.y (4 bytes)
	binary.BigEndian.PutUint32(b[14:18], uint32(y))
	
	// position.screen_width (2 bytes): 填 0 即可，伺服器端會自動適應
	binary.BigEndian.PutUint16(b[18:20], uint16(0))
	
	// position.screen_height (2 bytes): 填 0 即可
	binary.BigEndian.PutUint16(b[20:22], uint16(0))

	// pressure (4 bytes float32): 舊版是 0xffff，新版必須是 float32。
	// 1.0f 轉換為 IEEE 754 的十六進位表示法為 0x3f800000
	binary.BigEndian.PutUint32(b[22:26], 0x3f800000)

	// action_button (4 bytes): 預設填 0
	binary.BigEndian.PutUint32(b[26:30], 0)

	// buttons (4 bytes): 1 代表 AMOTION_EVENT_BUTTON_PRIMARY (主觸控/左鍵)
	binary.BigEndian.PutUint32(b[30:34], 1)

	return b
}

func (c *ScrcpyController) touch(action common.TouchAction, x, y int32, pointerID uint64) {
	c.Send(c.Encode(action, x, y, pointerID))
}

func (c *ScrcpyController) Down(pointerID uint64, x, y int) {
	c.touch(common.TouchDown, int32(x), int32(y), pointerID)
}

func (c *ScrcpyController) Move(pointerID uint64, x, y int) {
	c.touch(common.TouchMove, int32(x), int32(y), pointerID)
}

func (c *ScrcpyController) Up(pointerID uint64, x, y int) {
	c.touch(common.TouchUp, int32(x), int32(y), pointerID)
}

func (c *ScrcpyController) Close() error {
	c.cRunning = false
	c.vRunning = false

	if err := c.videoSocket.Close(); err != nil {
		return err
	}

	if err := c.controlSocket.Close(); err != nil {
		return err
	}

	return c.listener.Close()
}

func (c *ScrcpyController) Preprocess(rawEvents common.RawVirtualEvents, turnRight bool, dc *config.DeviceConfig, calc stage.JudgeLinePositionCalculator) []common.ViscousEventItem {
	width, height := float64(dc.Height), float64(dc.Width)
	x1, x2, yy := calc(width, height)
	mapper := func(x, y float64) (int, int) {
		return int(math.Round(x1 + (x2-x1)*x)), int(math.Round(yy - (yy-height/2)*y))
	}

	result := []common.ViscousEventItem{}
	currentFingers := make([]bool, 10)
	for _, events := range rawEvents {
		var data []byte
		for _, event := range events.Events {
			x, y := mapper(event.X, event.Y)
			switch event.Action {
			case common.TouchDown:
				if currentFingers[event.PointerID] {
					log.Fatalf("pointer `%d` is already on screen", event.PointerID)
				}
				currentFingers[event.PointerID] = true
			case common.TouchMove:
				if !currentFingers[event.PointerID] {
					log.Fatalf("pointer `%d` is not on screen", event.PointerID)
				}
			case common.TouchUp:
				if !currentFingers[event.PointerID] {
					log.Fatalf("pointer `%d` is not on screen", event.PointerID)
				}
				currentFingers[event.PointerID] = false
			default:
				log.Fatalf("unknown touch action: %d\n", event.Action)
			}

			data = append(data, c.Encode(event.Action, int32(x), int32(y), uint64(event.PointerID))...)
		}

		result = append(result, common.ViscousEventItem{
			Timestamp: events.Timestamp,
			Data:      data,
		})
	}

	return result
}

func (c *ScrcpyController) Send(data []byte) {
	n, err := c.controlSocket.Write(data)
	if err != nil {
		log.Fatalln("Failed to send control data through control socket:", err)
	}

	if n != len(data) {
		log.Fatalf("Failed to send control data through control socket: expect to send %d bytes, but %d bytes were sent", len(data), n)
	}
}
