package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
)

const (
	FrameData      = 0x01
	FrameHeartbeat = 0x02
	FrameClose     = 0x03
	FramePadding   = 0x04
)

func frameName(t byte) string {
	switch t {
	case FrameData:
		return "DATA"
	case FrameHeartbeat:
		return "HEARTBEAT"
	case FrameClose:
		return "CLOSE"
	case FramePadding:
		return "PADDING"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", t)
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newBoundary() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "----WebKitFormBoundary" + hex.EncodeToString(b)
}

type FrameLogger struct {
	Prefix       string
	LogHeartbeat bool
}

func (fl FrameLogger) LogSend(t byte, payloadLen int) {
	if t == FrameHeartbeat && !fl.LogHeartbeat {
		return
	}
	log.Printf("[%s] >>> %s | Payload=%d", fl.Prefix, frameName(t), payloadLen)
}

func (fl FrameLogger) LogRecv(t byte, payloadLen int) {
	if t == FrameHeartbeat && !fl.LogHeartbeat {
		return
	}
	log.Printf("[%s] <<< %s | Payload=%d", fl.Prefix, frameName(t), payloadLen)
}

func writeFrame(fl FrameLogger, w io.Writer, frameType byte, payload []byte) error {
	fl.LogSend(frameType, len(payload))

	header := []byte{frameType, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))

	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func readFrame(fl FrameLogger, r io.Reader) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	t := header[0]
	size := binary.BigEndian.Uint32(header[1:])
	fl.LogRecv(t, int(size))

	var payload []byte
	if size > 0 {
		payload = make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return t, nil, err
		}
	}
	return t, payload, nil
}
