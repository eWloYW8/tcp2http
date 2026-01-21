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
	id      string
	TCPConn net.Conn

	UpReady   chan struct{}
	DownReady chan struct{}

	mu      sync.Mutex
	UpBody  io.Reader
	closed  bool
	closeCh chan struct{}
}

func (t *Tunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	close(t.closeCh)
	if t.TCPConn != nil {
		_ = t.TCPConn.Close()
	}
}

type TunnelStore struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

func NewTunnelStore() *TunnelStore {
	return &TunnelStore{tunnels: make(map[string]*Tunnel)}
}

func (s *TunnelStore) GetOrCreate(id string, dialTarget string) (*Tunnel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.tunnels[id]; ok {
		return t, nil
	}

	conn, err := net.DialTimeout("tcp", dialTarget, 5*time.Second)
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
	cfg, err := parseServerFlags(args)
	if err != nil {
		return err
	}
	store := NewTunnelStore()
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.UpPath, func(w http.ResponseWriter, r *http.Request) { handleUp(cfg, store, w, r) })
	mux.HandleFunc(cfg.DownPath, func(w http.ResponseWriter, r *http.Request) { handleDown(cfg, store, w, r) })
	log.Printf("[server] listening on %s", cfg.HTTPListenAddr)
	return http.ListenAndServe(cfg.HTTPListenAddr, mux)
}

func handleUp(cfg *ServerConfig, store *TunnelStore, w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(cfg.SessionHeader)
	if id == "" {
		http.Error(w, "no session", 400)
		return
	}

	tunnel, err := store.GetOrCreate(id, cfg.TargetTCP)
	if err != nil {
		http.Error(w, "dial fail", 500)
		return
	}

	var body io.Reader = r.Body
	if cfg.AcceptMultipart && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		mr, _ := r.MultipartReader()
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FormName() == cfg.MultipartFormName {
				body = p
				break
			}
		}
	}

	tunnel.mu.Lock()
	tunnel.UpBody = body
	tunnel.mu.Unlock()

	select {
	case <-tunnel.UpReady:
	default:
		close(tunnel.UpReady)
	}

	// 等待 Down 侧就绪
	ctx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()

	select {
	case <-tunnel.DownReady:
	case <-ctx.Done():
		tunnel.Close()
		store.Delete(id)
		return
	}

	fl := FrameLogger{Prefix: "server-up"}
	for {
		t, payload, err := readFrame(fl, tunnel.UpBody)
		if err != nil {
			break
		}
		if t == FrameData {
			_, err = tunnel.TCPConn.Write(payload)
		} else if t == FrameClose {
			break
		}
		if err != nil {
			break
		}
	}
	tunnel.Close()
	store.Delete(id)
}

func handleDown(cfg *ServerConfig, store *TunnelStore, w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(cfg.SessionHeader)
	tunnel, err := store.GetOrCreate(id, cfg.TargetTCP)
	if err != nil {
		http.Error(w, "dial fail", 500)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}

	select {
	case <-tunnel.DownReady:
	default:
		close(tunnel.DownReady)
	}

	ctx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()

	select {
	case <-tunnel.UpReady:
	case <-ctx.Done():
		tunnel.Close()
		store.Delete(id)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 关键：立即刷新 Header

	readCh := make(chan []byte, 8)
	go func() {
		defer close(readCh)
		for {
			buf := make([]byte, cfg.BufferSize)
			n, err := tunnel.TCPConn.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case readCh <- data:
				case <-tunnel.closeCh:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	tk := time.NewTicker(cfg.HeartbeatInterval)
	defer tk.Stop()
	fl := FrameLogger{Prefix: "server-down"}

	for {
		select {
		case data, ok := <-readCh:
			if !ok {
				goto End
			}
			_ = writeFrame(fl, w, FrameData, data)
			if cfg.PaddingEnabled {
				_ = writeFrame(fl, w, FramePadding, make([]byte, cfg.PaddingSize))
			}
			flusher.Flush()
		case <-tk.C:
			_ = writeFrame(fl, w, FrameHeartbeat, nil)
			flusher.Flush()
		case <-r.Context().Done():
			goto End
		case <-tunnel.closeCh:
			goto End
		}
	}
End:
	_ = writeFrame(fl, w, FrameClose, nil)
	tunnel.Close()
	store.Delete(id)
}
