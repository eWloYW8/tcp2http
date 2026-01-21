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
	id        string
	TCPConn   net.Conn
	UpReady   chan io.ReadCloser
	DownReady chan *SafeWriter
	closeCh   chan struct{}
	once      sync.Once
}

func (t *Tunnel) Close() {
	t.once.Do(func() {
		close(t.closeCh)
		if t.TCPConn != nil {
			t.TCPConn.Close()
		}
	})
}

type TunnelStore struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

func NewTunnelStore() *TunnelStore { return &TunnelStore{tunnels: make(map[string]*Tunnel)} }

func (s *TunnelStore) GetOrCreate(id string, target string) (*Tunnel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tunnels[id]; ok {
		return t, nil
	}
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return nil, err
	}
	t := &Tunnel{
		id: id, TCPConn: conn,
		UpReady:   make(chan io.ReadCloser, 1),
		DownReady: make(chan *SafeWriter, 1),
		closeCh:   make(chan struct{}),
	}
	s.tunnels[id] = t
	return t, nil
}

func (s *TunnelStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tunnels[id]; ok {
		t.Close()
		delete(s.tunnels, id)
	}
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
	tunnel, err := store.GetOrCreate(id, cfg.TargetTCP)
	if err != nil {
		http.Error(w, "dial fail", 502)
		return
	}
	defer store.Delete(id)

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

	select {
	case tunnel.UpReady <- reader:
	default:
	}

	waitCtx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()
	select {
	case <-tunnel.DownReady:
	case <-waitCtx.Done():
		return
	case <-tunnel.closeCh:
		return
	}

	fl := FrameLogger{Prefix: "server-up"}
	for {
		t, payload, err := readFrame(fl, reader)
		if err != nil {
			break
		}
		if t == FrameData {
			if _, err := tunnel.TCPConn.Write(payload); err != nil {
				break
			}
		} else if t == FrameClose {
			break
		}
	}
}

func handleDown(cfg *ServerConfig, store *TunnelStore, w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(cfg.SessionHeader)
	tunnel, err := store.GetOrCreate(id, cfg.TargetTCP)
	if err != nil {
		http.Error(w, "dial fail", 502)
		return
	}
	defer store.Delete(id)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sw := NewSafeWriter(w)
	select {
	case tunnel.DownReady <- sw:
	default:
	}

	waitCtx, cancel := context.WithTimeout(r.Context(), cfg.WaitPeerTimeout)
	defer cancel()
	select {
	case <-tunnel.UpReady:
	case <-waitCtx.Done():
		return
	case <-tunnel.closeCh:
		return
	}

	fl := FrameLogger{Prefix: "server-down", LogHeartbeat: false}

	go func() {
		buf := make([]byte, cfg.BufferSize)
		for {
			n, err := tunnel.TCPConn.Read(buf)
			if n > 0 {
				sw.SetPriority(true)
				if e := sw.WriteFrame(fl, FrameData, buf[:n]); e != nil {
					sw.SetPriority(false)
					break
				}
				if cfg.PaddingMode == PaddingPerPacket {
					_ = sw.WriteFrame(fl, FramePadding, make([]byte, cfg.PaddingSize))
				}
				flusher.Flush()
				sw.SetPriority(false)
			}
			if err != nil {
				break
			}
		}
		_ = sw.WriteFrame(fl, FrameClose, nil)
		tunnel.Close()
	}()

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
			if err := sw.WriteFrame(fl, FrameHeartbeat, nil); err != nil {
				return
			}
			flusher.Flush()
		case <-padTicker.C:
			if sw.CheckAndResetDataSent() {
				if _, err := sw.TryWriteFrame(fl, FramePadding, make([]byte, cfg.PaddingSize)); err == nil {
					flusher.Flush()
				} else if err != nil {
					return
				}
			}
		case <-r.Context().Done():
			return
		case <-tunnel.closeCh:
			return
		}
	}
}
