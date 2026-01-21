package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"sync"
)

const (
	FrameData      = 0x01
	FrameHeartbeat = 0x02
	FrameClose     = 0x03
	FramePadding   = 0x04
	MaxFrameSize   = 10 * 1024 * 1024
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
	log.Printf("[%s] >>> %s | Len=%d", fl.Prefix, frameName(t), payloadLen)
}

func (fl FrameLogger) LogRecv(t byte, payloadLen int) {
	if t == FrameHeartbeat && !fl.LogHeartbeat {
		return
	}
	log.Printf("[%s] <<< %s | Len=%d", fl.Prefix, frameName(t), payloadLen)
}

type SafeWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewSafeWriter(w io.Writer) *SafeWriter { return &SafeWriter{w: w} }

func (sw *SafeWriter) WriteFrame(fl FrameLogger, t byte, payload []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	fl.LogSend(t, len(payload))
	header := make([]byte, 5)
	header[0] = t
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))

	if _, err := sw.w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := sw.w.Write(payload); err != nil {
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
	t, size := header[0], binary.BigEndian.Uint32(header[1:])
	if size > MaxFrameSize {
		return t, nil, fmt.Errorf("frame too large: %d", size)
	}
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
