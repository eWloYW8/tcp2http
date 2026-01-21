package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

func runClient(args []string) error {
	cfg, err := parseClientFlags(args)
	if err != nil {
		return err
	}
	session, err := newClientSession(cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Printf("[client] listening on %s", cfg.ListenAddr)

	go func() {
		<-session.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-session.Done():
				return session.Err()
			default:
			}
			continue
		}
		go session.HandleConn(conn)
	}
}

type clientChannel struct {
	id   uint32
	conn net.Conn
}

type ClientSession struct {
	cfg          *ClientConfig
	id           string
	ctx          context.Context
	cancel       context.CancelFunc
	httpClient   *http.Client
	upWriter     *SafeWriter
	upPipeWriter *io.PipeWriter
	boundary     string

	done    chan struct{}
	errOnce sync.Once
	err     error

	mu       sync.Mutex
	channels map[uint32]*clientChannel
	nextID   atomic.Uint32
}

func newClientSession(cfg *ClientConfig) (*ClientSession, error) {
	ctx, cancel := context.WithCancel(context.Background())
	session := &ClientSession{
		cfg:      cfg,
		id:       newSessionID(),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		channels: make(map[uint32]*clientChannel),
	}

	httpClient := &http.Client{}
	if cfg.HTTPTimeout > 0 {
		httpClient.Timeout = cfg.HTTPTimeout
	}
	session.httpClient = httpClient

	pr, pw := io.Pipe()
	session.upWriter = NewSafeWriter(pw)
	session.upPipeWriter = pw

	if cfg.Multipart {
		session.boundary = newBoundary()
	}

	reqUp, _ := http.NewRequestWithContext(ctx, "POST", cfg.UpURL, pr)
	reqUp.Header.Set(cfg.SessionHeader, session.id)
	reqUp.ContentLength = -1
	if cfg.Multipart {
		reqUp.Header.Set("Content-Type", "multipart/form-data; boundary="+session.boundary)
	} else {
		reqUp.Header.Set("Content-Type", "application/octet-stream")
	}
	_ = cfg.HeadersUp.Apply(reqUp.Header)

	reqDown, _ := http.NewRequestWithContext(ctx, "POST", cfg.DownURL, nil)
	reqDown.Header.Set(cfg.SessionHeader, session.id)
	_ = cfg.HeadersDown.Apply(reqDown.Header)

	go session.runUp(reqUp)
	go session.runDown(reqDown)
	go session.runKeepalive()

	if cfg.Multipart {
		h := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q; filename=%q\r\nContent-Type: application/octet-stream\r\n\r\n",
			session.boundary, cfg.MultipartFN, cfg.MultipartFF)
		if _, err := pw.Write([]byte(h)); err != nil {
			session.fail(err)
		}
	}

	return session, nil
}

func (s *ClientSession) runUp(reqUp *http.Request) {
	resp, err := s.httpClient.Do(reqUp)
	if err != nil {
		s.fail(err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	s.fail(fmt.Errorf("up-http-closed"))
}

func (s *ClientSession) runDown(reqDown *http.Request) {
	resp, err := s.httpClient.Do(reqDown)
	if err != nil {
		s.fail(err)
		return
	}
	defer resp.Body.Close()

	fl := FrameLogger{Prefix: "client-down", LogHeartbeat: s.cfg.LogHeartbeat}
	flUp := FrameLogger{Prefix: "client-up", LogHeartbeat: s.cfg.LogHeartbeat}
	for {
		t, channel, payload, err := readFrame(fl, resp.Body)
		if err != nil {
			break
		}
		switch t {
		case FrameData:
			if channel == 0 {
				continue
			}
			ch := s.getChannel(channel)
			if ch == nil {
				continue
			}
			if _, err := ch.conn.Write(payload); err != nil {
				s.closeChannel(channel)
				_ = s.upWriter.WriteFrame(flUp, FrameClose, channel, nil)
			}
		case FrameClose:
			if channel == 0 {
				continue
			}
			s.closeChannel(channel)
		case FrameHeartbeat, FramePadding, FrameOpen:
		}
	}
	s.fail(fmt.Errorf("down-closed"))
}

func (s *ClientSession) runKeepalive() {
	flUp := FrameLogger{Prefix: "client-up", LogHeartbeat: s.cfg.LogHeartbeat}
	hbTicker := &time.Ticker{C: make(<-chan time.Time)}
	if s.cfg.HeartbeatInterval > 0 {
		hbTicker = time.NewTicker(s.cfg.HeartbeatInterval)
	}

	padTicker := &time.Ticker{C: make(<-chan time.Time)}
	if s.cfg.PaddingMode == PaddingInterval && s.cfg.PaddingInterval > 0 {
		padTicker = time.NewTicker(s.cfg.PaddingInterval)
	}
	defer hbTicker.Stop()
	defer padTicker.Stop()

	for {
		select {
		case <-hbTicker.C:
			if err := s.upWriter.WriteFrame(flUp, FrameHeartbeat, 0, nil); err != nil {
				s.fail(err)
				return
			}
		case <-padTicker.C:
			if s.upWriter.CheckAndResetDataSent() {
				if _, err := s.upWriter.TryWriteFrame(flUp, FramePadding, 0, make([]byte, s.cfg.PaddingSize)); err != nil {
					s.fail(err)
					return
				}
			}
		case <-s.Done():
			return
		}
	}
}

func (s *ClientSession) HandleConn(conn net.Conn) {
	select {
	case <-s.Done():
		_ = conn.Close()
		return
	default:
	}

	channelID := s.nextID.Add(1)
	s.addChannel(channelID, conn)

	flUp := FrameLogger{Prefix: "client-up", LogHeartbeat: s.cfg.LogHeartbeat}
	if err := s.upWriter.WriteFrame(flUp, FrameOpen, channelID, nil); err != nil {
		s.removeChannel(channelID)
		_ = conn.Close()
		s.fail(err)
		return
	}

	go s.pipeConn(channelID, conn)
}

func (s *ClientSession) pipeConn(channelID uint32, conn net.Conn) {
	defer conn.Close()

	flUp := FrameLogger{Prefix: "client-up", LogHeartbeat: s.cfg.LogHeartbeat}
	buf := make([]byte, s.cfg.BufferSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			s.upWriter.SetPriority(true)
			if e := s.upWriter.WriteFrame(flUp, FrameData, channelID, buf[:n]); e != nil {
				s.upWriter.SetPriority(false)
				s.fail(e)
				return
			}
			if s.cfg.PaddingMode == PaddingPerPacket {
				_ = s.upWriter.WriteFrame(flUp, FramePadding, 0, make([]byte, s.cfg.PaddingSize))
			}
			s.upWriter.SetPriority(false)
		}
		if err != nil {
			break
		}
	}

	if s.removeChannel(channelID) {
		_ = s.upWriter.WriteFrame(flUp, FrameClose, channelID, nil)
	}
}

func (s *ClientSession) fail(err error) {
	s.errOnce.Do(func() {
		if err == nil {
			err = fmt.Errorf("session closed")
		}
		s.err = err
		close(s.done)
		s.cancel()
		if s.cfg.Multipart && s.boundary != "" && s.upPipeWriter != nil {
			_, _ = s.upPipeWriter.Write([]byte("\r\n--" + s.boundary + "--\r\n"))
		}
		if s.upPipeWriter != nil {
			_ = s.upPipeWriter.Close()
		}
		s.closeAllChannels()
	})
}

func (s *ClientSession) Done() <-chan struct{} {
	return s.done
}

func (s *ClientSession) Err() error {
	return s.err
}

func (s *ClientSession) addChannel(id uint32, conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[id] = &clientChannel{id: id, conn: conn}
}

func (s *ClientSession) getChannel(id uint32) *clientChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channels[id]
}

func (s *ClientSession) removeChannel(id uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.channels[id]; ok {
		delete(s.channels, id)
		return true
	}
	return false
}

func (s *ClientSession) closeChannel(id uint32) {
	s.mu.Lock()
	ch, ok := s.channels[id]
	if ok {
		delete(s.channels, id)
	}
	s.mu.Unlock()
	if ok {
		_ = ch.conn.Close()
	}
}

func (s *ClientSession) closeAllChannels() {
	s.mu.Lock()
	channels := s.channels
	s.channels = make(map[uint32]*clientChannel)
	s.mu.Unlock()

	for _, ch := range channels {
		_ = ch.conn.Close()
	}
}
