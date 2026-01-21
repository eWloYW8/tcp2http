package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Tunnel struct {
	id string

	TCPConn net.Conn

	UpBody io.Reader

	DownW     http.ResponseWriter
	DownFlush http.Flusher

	UpReady   chan struct{}
	DownReady chan struct{}

	closeOnce sync.Once
	closeCh   chan struct{}
}

func (t *Tunnel) Close() {
	t.closeOnce.Do(func() {
		close(t.closeCh)
		if t.TCPConn != nil {
			_ = t.TCPConn.Close()
		}
	})
}

type TunnelStore struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

func NewTunnelStore() *TunnelStore {
	return &TunnelStore{tunnels: make(map[string]*Tunnel)}
}

func (s *TunnelStore) Get(id string) (*Tunnel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tunnels[id]
	return t, ok
}

func (s *TunnelStore) GetOrCreate(id string, dialTarget string) (*Tunnel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.tunnels[id]; ok {
		return t, nil
	}

	conn, err := net.Dial("tcp", dialTarget)
	if err != nil {
		return nil, err
	}

	t := &Tunnel{
		id:        id,
		TCPConn:   conn,
		UpReady:   make(chan struct{}),
		DownReady: make(chan struct{}),
		closeCh:   make(chan struct{}),
	}
	s.tunnels[id] = t
	return t, nil
}

func (s *TunnelStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tunnels, id)
}

func runServer(args []string) error {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := parseServerFlags(args)
	if err != nil {
		return err
	}

	store := NewTunnelStore()

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.UpPath, func(w http.ResponseWriter, r *http.Request) {
		handleUp(cfg, store, w, r)
	})
	mux.HandleFunc(cfg.DownPath, func(w http.ResponseWriter, r *http.Request) {
		handleDown(cfg, store, w, r)
	})

	srv := &http.Server{
		Addr:    cfg.HTTPListenAddr,
		Handler: mux,
	}

	log.Printf("[server] listening on %s (up=%s down=%s target=%s)", cfg.HTTPListenAddr, cfg.UpPath, cfg.DownPath, cfg.TargetTCP)
	return srv.ListenAndServe()
}

func handleUp(cfg *ServerConfig, store *TunnelStore, w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.Header.Get(cfg.SessionHeader))
	if id == "" {
		http.Error(w, "missing session header", http.StatusBadRequest)
		return
	}
	log.Printf("[server-up] session=%s from=%s content-length=%d", id, r.RemoteAddr, r.ContentLength)

	tunnel, err := store.GetOrCreate(id, cfg.TargetTCP)
	if err != nil {
		log.Printf("[server-up] session=%s dial target failed: %v", id, err)
		http.Error(w, "tcp connect failed", http.StatusInternalServerError)
		return
	}
	log.Printf("[server-up] session=%s target=%s", id, tunnel.TCPConn.RemoteAddr())

	// Select body reader (multipart or raw)
	var body io.Reader
	ct := r.Header.Get("Content-Type")

	if cfg.AcceptMultipart && strings.HasPrefix(ct, "multipart/form-data") {
		mr, err := r.MultipartReader()
		if err != nil {
			log.Printf("[server-up] session=%s multipart reader error: %v", id, err)
			http.Error(w, "multipart error", http.StatusBadRequest)
			return
		}
		found := false
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("[server-up] session=%s next part error: %v", id, err)
				break
			}
			if part.FormName() == cfg.MultipartFormName {
				body = part
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "multipart file part not found", http.StatusBadRequest)
			tunnel.Close()
			store.Delete(id)
			return
		}
	} else {
		body = r.Body
	}

	tunnel.UpBody = body

	// signal UpReady
	select {
	case <-tunnel.UpReady:
	default:
		close(tunnel.UpReady)
	}

	// wait for DownReady
	ctx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()

	select {
	case <-tunnel.DownReady:
	case <-tunnel.closeCh:
		http.Error(w, "tunnel closed", http.StatusGone)
		store.Delete(id)
		return
	case <-ctx.Done():
		http.Error(w, "timeout waiting for /down", http.StatusGatewayTimeout)
		tunnel.Close()
		store.Delete(id)
		return
	}

	fl := FrameLogger{Prefix: "server-up", LogHeartbeat: false}

	bufToTCP := func(p []byte) error {
		_, err := tunnel.TCPConn.Write(p)
		return err
	}

ReadLoop:
	for {
		select {
		case <-tunnel.closeCh:
			store.Delete(id)
			return
		default:
		}

		t, payload, err := readFrame(fl, tunnel.UpBody)
		if err != nil {
			break ReadLoop
		}

		switch t {
		case FrameData:
			if err := bufToTCP(payload); err != nil {
				break ReadLoop
			}
		case FramePadding:
			// drop
		case FrameHeartbeat:
			// ignore
		case FrameClose:
			tunnel.Close()
			store.Delete(id)
			return
		default:
			// ignore unknown
		}
	}

	tunnel.Close()
	store.Delete(id)
	log.Printf("[server-up] session=%s exited", id)
}

func handleDown(cfg *ServerConfig, store *TunnelStore, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimSpace(r.Header.Get(cfg.SessionHeader))
	if id == "" {
		http.Error(w, "missing session header", http.StatusBadRequest)
		return
	}
	log.Printf("[server-down] session=%s from=%s", id, r.RemoteAddr)

	tunnel, err := store.GetOrCreate(id, cfg.TargetTCP)
	if err != nil {
		log.Printf("[server-down] session=%s dial target failed: %v", id, err)
		http.Error(w, "tcp connect failed", http.StatusInternalServerError)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		tunnel.Close()
		store.Delete(id)
		return
	}

	// signal DownReady
	select {
	case <-tunnel.DownReady:
	default:
		close(tunnel.DownReady)
	}

	// wait for UpReady BEFORE writing 200
	ctx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()

	select {
	case <-tunnel.UpReady:
		// ok
	case <-tunnel.closeCh:
		http.Error(w, "tunnel closed", http.StatusGone)
		store.Delete(id)
		return
	case <-ctx.Done():
		http.Error(w, "timeout waiting for /up", http.StatusGatewayTimeout)
		tunnel.Close()
		store.Delete(id)
		return
	}

	// now it's safe to write headers + 200
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	tunnel.DownW = w
	tunnel.DownFlush = flusher

	frameLog := FrameLogger{Prefix: "server-down", LogHeartbeat: false}

	// reader: TCP -> channel
	type readResult struct {
		n   int
		err error
		buf []byte
	}
	readCh := make(chan readResult, 8)

	go func() {
		defer close(readCh)
		for {
			buf := make([]byte, cfg.BufferSize)
			n, err := tunnel.TCPConn.Read(buf)
			readCh <- readResult{n: n, err: err, buf: buf}
			if err != nil {
				return
			}
		}
	}()

	var paddingBuf []byte
	if cfg.PaddingEnabled && cfg.PaddingSize > 0 {
		paddingBuf = make([]byte, cfg.PaddingSize)
	}

	var hbTick *time.Ticker
	if cfg.HeartbeatInterval > 0 {
		hbTick = time.NewTicker(cfg.HeartbeatInterval)
		defer hbTick.Stop()
	}

	writeAndFlush := func(t byte, payload []byte) error {
		if err := writeFrame(frameLog, w, t, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	for {
		select {
		case <-tunnel.closeCh:
			store.Delete(id)
			return

		case res, ok := <-readCh:
			if !ok {
				goto End
			}
			if res.n > 0 {
				if err := writeAndFlush(FrameData, res.buf[:res.n]); err != nil {
					goto End
				}
				if cfg.PaddingEnabled && len(paddingBuf) > 0 {
					if err := writeAndFlush(FramePadding, paddingBuf); err != nil {
						goto End
					}
				}
			}
			if res.err != nil {
				goto End
			}

		case <-func() <-chan time.Time {
			if hbTick == nil {
				return nil
			}
			return hbTick.C
		}():
			_ = writeAndFlush(FrameHeartbeat, nil)

		case <-r.Context().Done():
			goto End
		}
	}

End:
	_ = writeAndFlush(FrameClose, nil)
	tunnel.Close()
	store.Delete(id)
	log.Printf("[server-down] session=%s exited", id)
}
