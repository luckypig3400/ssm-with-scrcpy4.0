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
	return &ScrcpyController{\n\t\tdevice:    device,
		sessionID: fmt.Sprintf("%08x", rand.Int31()),
	}
}

func tryListen(host string, startPort int, endPort int) (net.Listener, int, error) {
	for port := startPort; port <= endPort; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
		if err == nil {
			return l, port, nil
		}
	}
	return nil, 0, fmt.Errorf("no free port in range %d-%d", startPort, endPort)
}

func (c *ScrcpyController) Open(filepath string, version string) error {
	f, err := os.Open(filepath)
	if err != nil {
		return err
	}
	defer f.Close()

	localName := fmt.Sprintf("localabstract:scrcpy_%s", c.sessionID)
	listener, port, err := tryListen("127.0.0.1", 27183, 27198)
	if err != nil {
		log.Fatalln("Failed to listen on local port:", err)
	}
	c.listener = listener

	if err := c.device.Forward(fmt.Sprintf("tcp:%d", port), localName); err != nil {
		log.Fatalln("Failed to forward port:", err)
	}

	log.Debugln("`scrcpy-server` loaded.")

	if err := c.device.Push(f, "/data/local/tmp/scrcpy-server.jar"); err != nil {
		log.Fatalln("Failed to push `scrcpy-server`:", err)
	}

	log.Debugln("`scrcpy-server` pushed to gaming device.")

	go func() {
		args := []string{
			"shell",
			"CLASSPATH=/data/local/tmp/scrcpy-server.jar",
			"app_process",
			"/",
			"com.genymobile.scrcpy.Server",
			version,
			"tunnel_forward=true",
			"audio=false",
			"control=true",
			"video_bit_rate=4000000",
			"max_fps=60",
			"lock_video_orientation=-1",
			"stay_awake=true",
			"power_off_on_close=false",
			"clipboard_autosync=false",
		}
		_, err := c.device.Run(args...)
		if err != nil {
			log.Fatalln("Failed to start `scrcpy-server`:", err)
		}
	}()

	videoSocket, err := listener.Accept()
	if err != nil {
		log.Fatalln("Failed to accept video socket:", err)
	}
	c.videoSocket = videoSocket
	log.Debugln("Video socket connected.")

	controlSocket, err := listener.Accept()
	if err != nil {
		log.Fatalln("Failed to accept control socket:", err)
	}
	c.controlSocket = controlSocket
	log.Debugln("Control socket connected.")

	dummy := make([]byte, 1)
	_, err = videoSocket.Read(dummy)
	if err != nil {
		log.Fatalln("Failed to read dummy byte from video socket:", err)
	}

	deviceNameBuf := make([]byte, 64)
	_, err = videoSocket.Read(deviceNameBuf)
	if err != nil {
		log.Fatalln("Failed to read device name from video socket:", err)
	}

	codecIDBuf := make([]byte, 4)
	_, err = videoSocket.Read(codecIDBuf)
	if err != nil {
		log.Fatalln("Failed to read codec ID from video socket:", err)
	}
	c.codecID = string(codecIDBuf)

	widthBuf := make([]byte, 4)
	_, err = videoSocket.Read(widthBuf)
	if err != nil {
		log.Fatalln("Failed to read width from video socket:", err)
	}
	c.width = int(binary.BigEndian.Uint32(widthBuf))

	heightBuf := make([]byte, 4)
	_, err = videoSocket.Read(heightBuf)
	if err != nil {
		log.Fatalln("Failed to read height from video socket:", err)
	}
	c.height = int(binary.BigEndian.Uint32(heightBuf))

	log.Infof("Connected to device, resolution: %dx%d, codec: %s\n", c.width, c.height, c.codecID)

	decoder, err := av.NewAVDecoder(c.codecID)
	if err != nil {
		log.Fatalln("Failed to create AV decoder:", err)
	}
	c.decoder = decoder

	c.cRunning = true
	c.vRunning = true

	go func() {
		for c.vRunning {
			headerBuf := make([]byte, 12)
			_, err := videoSocket.Read(headerBuf)
			if err != nil {
				break
			}
			pts := binary.BigEndian.Uint64(headerBuf[0:8])
			length := binary.BigEndian.Uint32(headerBuf[8:12])

			dataBuf := make([]byte, length)
			_, err = videoSocket.Read(dataBuf)
			if err != nil {
				break
			}

			err = c.decoder.Decode(pts, dataBuf)
			if err != nil {
				log.Debugln("Failed to decode frame:", err)
			}
		}
	}()

	return nil
}

// Encode 將觸控動作序列化為符合 scrcpy-server v4.0 協定的 42-byte 封包
func (c *ScrcpyController) Encode(action common.TouchAction, x, y int32, pointerID uint64) []byte {
	// scrcpy v3.0 / v4.0 觸控封包標準長度為 42 bytes
	b := make([]byte, 42)
	
	// 1. type (1 byte): 2 代表 CONTROL_MSG_TYPE_INJECT_TOUCH_EVENT
	b[0] = 2 
	
	// 2. action (1 byte): 0=Down, 1=Up, 2=Move 等
	b[1] = byte(action)

	// 3. pointer_id (8 bytes)
	binary.BigEndian.PutUint64(b[2:10], pointerID)

	// 4. position.x (4 bytes)
	binary.BigEndian.PutUint32(b[10:14], uint32(x))
	
	// 5. position.y (4 bytes)
	binary.BigEndian.PutUint32(b[14:18], uint32(y))
	
	// 6. position.screen_width (2 bytes): 傳入經 SSM 記錄的實際寬度，確保坐標對齊
	binary.BigEndian.PutUint16(b[18:20], uint16(c.width))
	
	// 7. position.screen_height (2 bytes): 傳入實際高度
	binary.BigEndian.PutUint16(b[20:22], uint16(c.height))

	// 8. pressure (4 bytes float32): 新版採用 IEEE 754 float32 格式。
	// 填入最大壓力值 1.0f，其十六進位值固定為 0x3f800000
	binary.BigEndian.PutUint32(b[22:26], 0x3f800000)

	// 9. action_button (4 bytes): 預設填 0
	binary.BigEndian.PutUint32(b[26:30], 0)

	// 10. buttons (4 bytes): 1 代表 AMOTION_EVENT_BUTTON_PRIMARY (主點擊觸控)
	binary.BigEndian.PutUint32(b[30:34], 1)

	// 11. tilt_x (4 bytes float32): 電磁筆傾斜度X，普通手指觸控填 0.0f (0x00000000)
	binary.BigEndian.PutUint32(b[34:38], 0)

	// 12. tilt_y (4 bytes float32): 電磁筆傾斜度Y，普通手指觸控填 0.0f (0x00000000)
	binary.BigEndian.PutUint32(b[38:42], 0)

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
	if c.videoSocket != nil {
		c.videoSocket.Close()
	}
	if c.controlSocket != nil {
		c.controlSocket.Close()
	}
	if c.listener != nil {
		c.listener.Close()
	}
	if c.decoder != nil {
		c.decoder.Drop()
	}
	localName := fmt.Sprintf("localabstract:scrcpy_%s", c.sessionID)
	_ = c.device.ForwardRemove(localName)
	return nil
}

func (c *ScrcpyController) Preprocess(rawEvents common.RawVirtualEvents, turnRight bool, dc *config.DeviceConfig, calc stage.JudgeLinePositionCalculator) []common.ViscousEventItem {
	x1, x2, yy := calc()

	width := c.width
	height := c.height
	if turnRight {
		width = c.height
		height = c.width
	}

	mapper := func(x, y float64) (int, int) {
		if turnRight {
			return int(math.Round(yy - (yy-height/2)*y)), int(math.Round(x1 + (x2-x1)*x))
		}
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
			Time: events.Time,
			Data: data,
		})
	}

	return result
}

func (c *ScrcpyController) Send(data []byte) {
	if c.controlSocket == nil {
		return
	}
	_, err := c.controlSocket.Write(data)
	if err != nil {
		log.Fatalln("Failed to send control data through control socket:", err)
	}
}