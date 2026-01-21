package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

const (
	FrameData      = 0x01
	FrameHeartbeat = 0x02
	FrameClose     = 0x03
	FramePadding   = 0x04
	FrameOpen      = 0x05
	MaxFrameSize   = 10 * 1024 * 1024
)

const (
	PaddingNone      = "none"
	PaddingPerPacket = "per-packet"
	PaddingInterval  = "interval"
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
	case FrameOpen:
		return "OPEN"
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

func (fl FrameLogger) LogSend(t byte, channel uint32, payloadLen int) {
	if t == FrameHeartbeat && !fl.LogHeartbeat {
		return
	}
	logDebugf("[%s] >>> %s | Ch=%d | Len=%d", fl.Prefix, frameName(t), channel, payloadLen)
}

func (fl FrameLogger) LogRecv(t byte, channel uint32, payloadLen int) {
	if t == FrameHeartbeat && !fl.LogHeartbeat {
		return
	}
	logDebugf("[%s] <<< %s | Ch=%d | Len=%d", fl.Prefix, frameName(t), channel, payloadLen)
}

type SafeWriter struct {
	mu                 sync.Mutex
	w                  io.Writer
	priorityWaiting    atomic.Int32
	dataSentInInterval atomic.Bool
}

func NewSafeWriter(w io.Writer) *SafeWriter { return &SafeWriter{w: w} }

func (sw *SafeWriter) SetPriority(active bool) {
	if active {
		sw.priorityWaiting.Add(1)
	} else {
		sw.priorityWaiting.Add(-1)
	}
}

func (sw *SafeWriter) CheckAndResetDataSent() bool {
	return sw.dataSentInInterval.Swap(false)
}

func (sw *SafeWriter) WriteFrame(fl FrameLogger, t byte, channel uint32, payload []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.writeRaw(fl, t, channel, payload)
}

func (sw *SafeWriter) TryWriteFrame(fl FrameLogger, t byte, channel uint32, payload []byte) (bool, error) {
	if sw.priorityWaiting.Load() > 0 {
		return false, nil
	}
	if !sw.mu.TryLock() {
		return false, nil
	}
	defer sw.mu.Unlock()
	err := sw.writeRaw(fl, t, channel, payload)
	return true, err
}

func (sw *SafeWriter) writeRaw(fl FrameLogger, t byte, channel uint32, payload []byte) error {
	if t == FrameData {
		sw.dataSentInInterval.Store(true)
	}

	fl.LogSend(t, channel, len(payload))
	header := make([]byte, 9)
	header[0] = t
	binary.BigEndian.PutUint32(header[1:5], channel)
	binary.BigEndian.PutUint32(header[5:], uint32(len(payload)))
	if _, err := sw.w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := sw.w.Write(payload)
		return err
	}
	return nil
}

func readFrame(fl FrameLogger, r io.Reader) (byte, uint32, []byte, error) {
	header := make([]byte, 9)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, 0, nil, err
	}
	t := header[0]
	channel := binary.BigEndian.Uint32(header[1:5])
	size := binary.BigEndian.Uint32(header[5:])
	if size > MaxFrameSize {
		return t, channel, nil, fmt.Errorf("frame too large: %d", size)
	}
	fl.LogRecv(t, channel, int(size))
	var payload []byte
	if size > 0 {
		payload = make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return t, channel, nil, err
		}
	}
	return t, channel, payload, nil
}
