package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type SessionWriter struct {
	*SafeWriter
	flusher http.Flusher
}

func NewSessionWriter(w io.Writer, flusher http.Flusher) *SessionWriter {
	return &SessionWriter{SafeWriter: NewSafeWriter(w), flusher: flusher}
}

func (w *SessionWriter) WriteFrame(fl FrameLogger, t byte, channel uint32, payload []byte) error {
	if err := w.SafeWriter.WriteFrame(fl, t, channel, payload); err != nil {
		return err
	}
	w.flusher.Flush()
	return nil
}

func (w *SessionWriter) TryWriteFrame(fl FrameLogger, t byte, channel uint32, payload []byte) (bool, error) {
	ok, err := w.SafeWriter.TryWriteFrame(fl, t, channel, payload)
	if ok && err == nil {
		w.flusher.Flush()
	}
	return ok, err
}

type ServerChannel struct {
	id   uint32
	conn net.Conn
}

type ServerSession struct {
	id        string
	upReady   chan struct{}
	downReady chan struct{}
	closeCh   chan struct{}
	once      sync.Once
	upOnce    sync.Once
	downOnce  sync.Once

	mu         sync.Mutex
	channels   map[uint32]*ServerChannel
	downWriter *SessionWriter
	upCloser   io.Closer
}

func newServerSession(id string) *ServerSession {
	return &ServerSession{
		id:        id,
		upReady:   make(chan struct{}),
		downReady: make(chan struct{}),
		closeCh:   make(chan struct{}),
		channels:  make(map[uint32]*ServerChannel),
	}
}

func (s *ServerSession) SetUpReady(reader io.ReadCloser) {
	s.mu.Lock()
	s.upCloser = reader
	s.mu.Unlock()
	s.upOnce.Do(func() { close(s.upReady) })
}

func (s *ServerSession) SetDownReady(writer *SessionWriter) {
	s.mu.Lock()
	s.downWriter = writer
	s.mu.Unlock()
	s.downOnce.Do(func() { close(s.downReady) })
}

func (s *ServerSession) DownWriter() *SessionWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.downWriter
}

func (s *ServerSession) Close() {
	s.once.Do(func() {
		close(s.closeCh)
		s.mu.Lock()
		if s.upCloser != nil {
			_ = s.upCloser.Close()
		}
		for _, ch := range s.channels {
			_ = ch.conn.Close()
		}
		s.channels = make(map[uint32]*ServerChannel)
		s.mu.Unlock()
	})
}

func (s *ServerSession) AddChannel(id uint32, conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.channels[id]; ok {
		return false
	}
	s.channels[id] = &ServerChannel{id: id, conn: conn}
	return true
}

func (s *ServerSession) GetChannel(id uint32) *ServerChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channels[id]
}

func (s *ServerSession) RemoveChannel(id uint32) *ServerChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.channels[id]
	delete(s.channels, id)
	return ch
}

func (s *ServerSession) CloseChannel(id uint32) {
	ch := s.RemoveChannel(id)
	if ch != nil {
		_ = ch.conn.Close()
	}
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*ServerSession
}

func NewSessionStore() *SessionStore { return &SessionStore{sessions: make(map[string]*ServerSession)} }

func (s *SessionStore) GetOrCreate(id string) *ServerSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[id]; ok {
		return session
	}
	session := newServerSession(id)
	s.sessions[id] = session
	return session
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if ok {
		session.Close()
	}
}

func runServer(args []string) error {
	cfg, err := parseServerFlags(args)
	if err != nil {
		return err
	}
	setLogLevel(cfg.LogLevel)
	store := NewSessionStore()
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.UpPath, func(w http.ResponseWriter, r *http.Request) { handleUp(cfg, store, w, r) })
	mux.HandleFunc(cfg.DownPath, func(w http.ResponseWriter, r *http.Request) { handleDown(cfg, store, w, r) })
	logInfof("[server] listening on %s", cfg.HTTPListenAddr)
	return http.ListenAndServe(cfg.HTTPListenAddr, mux)
}

func handleUp(cfg *ServerConfig, store *SessionStore, w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(cfg.SessionHeader)
	session := store.GetOrCreate(id)
	defer store.Delete(id)
	logInfof("[server] session=%s up connected", id)
	defer logInfof("[server] session=%s up disconnected", id)

	var reader io.ReadCloser = r.Body
	if cfg.AcceptMultipart && strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		mr, _ := r.MultipartReader()
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FormName() == cfg.MultipartFormName {
				reader = p
				break
			}
		}
	}
	session.SetUpReady(reader)

	waitCtx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()
	select {
	case <-session.downReady:
	case <-waitCtx.Done():
		logWarnf("[server] session=%s up wait down timeout", id)
		return
	case <-session.closeCh:
		logWarnf("[server] session=%s up closed before down ready", id)
		return
	}

	flUp := FrameLogger{Prefix: "server-up"}
	flDown := FrameLogger{Prefix: "server-down", LogHeartbeat: false}

	for {
		t, channel, payload, err := readFrame(flUp, reader)
		if err != nil {
			break
		}
		switch t {
		case FrameOpen:
			if channel == 0 {
				continue
			}
			if session.GetChannel(channel) != nil {
				logWarnf("[server] session=%s channel=%d open duplicated", id, channel)
				continue
			}
			conn, err := net.DialTimeout("tcp", cfg.TargetTCP, 5*time.Second)
			if err != nil {
				logErrorf("[server] session=%s channel=%d dial %s error: %v", id, channel, cfg.TargetTCP, err)
				if sw := session.DownWriter(); sw != nil {
					_ = sw.WriteFrame(flDown, FrameClose, channel, nil)
				}
				continue
			}
			if !session.AddChannel(channel, conn) {
				_ = conn.Close()
				continue
			}
			logInfof("[server] session=%s channel=%d opened to %s", id, channel, cfg.TargetTCP)
			startServerChannel(cfg, session, channel)
		case FrameData:
			if channel == 0 {
				continue
			}
			ch := session.GetChannel(channel)
			if ch == nil {
				logWarnf("[server] session=%s data for unknown channel=%d", id, channel)
				continue
			}
			if _, err := ch.conn.Write(payload); err != nil {
				logWarnf("[server] session=%s channel=%d write error: %v", id, channel, err)
				session.CloseChannel(channel)
				if sw := session.DownWriter(); sw != nil {
					_ = sw.WriteFrame(flDown, FrameClose, channel, nil)
				}
			}
		case FrameClose:
			if channel == 0 {
				continue
			}
			logInfof("[server] session=%s channel=%d closed by client", id, channel)
			session.CloseChannel(channel)
		case FrameHeartbeat, FramePadding:
		}
	}
}

func handleDown(cfg *ServerConfig, store *SessionStore, w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(cfg.SessionHeader)
	session := store.GetOrCreate(id)
	defer store.Delete(id)
	logInfof("[server] session=%s down connected", id)
	defer logInfof("[server] session=%s down disconnected", id)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sw := NewSessionWriter(w, flusher)
	session.SetDownReady(sw)

	waitCtx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()
	select {
	case <-session.upReady:
	case <-waitCtx.Done():
		logWarnf("[server] session=%s down wait up timeout", id)
		return
	case <-session.closeCh:
		logWarnf("[server] session=%s down closed before up ready", id)
		return
	}

	go runDownKeepalive(cfg, session, sw, r.Context())

	select {
	case <-session.closeCh:
	case <-r.Context().Done():
	}
}

func startServerChannel(cfg *ServerConfig, session *ServerSession, channelID uint32) {
	ch := session.GetChannel(channelID)
	if ch == nil {
		return
	}
	downWriter := session.DownWriter()
	if downWriter == nil {
		return
	}

	go func() {
		defer ch.conn.Close()

		fl := FrameLogger{Prefix: "server-down", LogHeartbeat: false}
		buf := make([]byte, cfg.BufferSize)
		for {
			n, err := ch.conn.Read(buf)
			if n > 0 {
				downWriter.SetPriority(true)
				if e := downWriter.WriteFrame(fl, FrameData, channelID, buf[:n]); e != nil {
					downWriter.SetPriority(false)
					logErrorf("[server] session=%s channel=%d down write error: %v", session.id, channelID, e)
					session.Close()
					return
				}
				if cfg.PaddingMode == PaddingPerPacket {
					_ = downWriter.WriteFrame(fl, FramePadding, 0, make([]byte, cfg.PaddingSize))
				}
				downWriter.SetPriority(false)
			}
			if err != nil {
				if err != io.EOF {
					logWarnf("[server] session=%s channel=%d tcp read error: %v", session.id, channelID, err)
				}
				break
			}
		}

		logInfof("[server] session=%s channel=%d closed by target", session.id, channelID)
		_ = downWriter.WriteFrame(fl, FrameClose, channelID, nil)
		session.RemoveChannel(channelID)
	}()
}

func runDownKeepalive(cfg *ServerConfig, session *ServerSession, sw *SessionWriter, ctx context.Context) {
	fl := FrameLogger{Prefix: "server-down", LogHeartbeat: false}
	hbTicker := &time.Ticker{C: make(<-chan time.Time)}
	if cfg.HeartbeatInterval > 0 {
		hbTicker = time.NewTicker(cfg.HeartbeatInterval)
	}

	padTicker := &time.Ticker{C: make(<-chan time.Time)}
	if cfg.PaddingMode == PaddingInterval && cfg.PaddingInterval > 0 {
		padTicker = time.NewTicker(cfg.PaddingInterval)
	}
	defer hbTicker.Stop()
	defer padTicker.Stop()

	for {
		select {
		case <-hbTicker.C:
			if err := sw.WriteFrame(fl, FrameHeartbeat, 0, nil); err != nil {
				session.Close()
				return
			}
		case <-padTicker.C:
			if sw.CheckAndResetDataSent() {
				if _, err := sw.TryWriteFrame(fl, FramePadding, 0, make([]byte, cfg.PaddingSize)); err != nil {
					session.Close()
					return
				}
			}
		case <-session.closeCh:
			return
		case <-ctx.Done():
			return
		}
	}
}
