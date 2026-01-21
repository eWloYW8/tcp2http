package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

func runClient(args []string) error {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := parseClientFlags(args)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	log.Printf("[client] listening on %s", cfg.ListenAddr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[client] accept error: %v", err)
			continue
		}
		go handleClientConn(cfg, conn)
	}
}

func handleClientConn(cfg *ClientConfig, tcpConn net.Conn) {
	defer tcpConn.Close()

	sessionID := newSessionID()
	log.Printf("[client] new session=%s from=%s", sessionID, tcpConn.RemoteAddr())

	httpClient := &http.Client{}
	if cfg.HTTPTimeout > 0 {
		httpClient.Timeout = cfg.HTTPTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prUp, pwUp := io.Pipe()

	var multipartBoundary string
	if cfg.Multipart {
		multipartBoundary = newBoundary()
	}

	reqUp, err := http.NewRequestWithContext(ctx, "POST", cfg.UpURL, prUp)
	if err != nil {
		log.Printf("[client] session=%s build up request error: %v", sessionID, err)
		_ = pwUp.Close()
		return
	}
	reqUp.Header.Set(cfg.SessionHeader, sessionID)
	reqUp.ContentLength = -1

	if cfg.Multipart {
		reqUp.Header.Set("Content-Type", "multipart/form-data; boundary="+multipartBoundary)
	} else {
		reqUp.Header.Set("Content-Type", "application/octet-stream")
	}
	_ = cfg.HeadersUp.Apply(reqUp.Header)

	reqDown, err := http.NewRequestWithContext(ctx, "POST", cfg.DownURL, nil)
	if err != nil {
		log.Printf("[client] session=%s build down request error: %v", sessionID, err)
		_ = pwUp.Close()
		return
	}
	reqDown.Header.Set(cfg.SessionHeader, sessionID)
	_ = cfg.HeadersDown.Apply(reqDown.Header)

	errCh := make(chan error, 3)

	// 1) /up http goroutine
	go func() {
		resp, err := httpClient.Do(reqUp)
		if err != nil {
			errCh <- fmt.Errorf("up-http: %w", err)
			return
		}
		defer resp.Body.Close()
		log.Printf("[client-up-http] session=%s status=%s", sessionID, resp.Status)
		_, _ = io.Copy(io.Discard, resp.Body)
		errCh <- fmt.Errorf("up-http: closed")
	}()

	// 2) /up writer goroutine
	go func() {
		defer func() {
			_ = writeFrame(FrameLogger{Prefix: "client-up", LogHeartbeat: cfg.LogHeartbeat}, pwUp, FrameClose, nil)
			if cfg.Multipart {
				end := fmt.Sprintf("\r\n--%s--\r\n", multipartBoundary)
				_, _ = pwUp.Write([]byte(end))
			}
			_ = pwUp.Close()
		}()

		if cfg.Multipart {
			partHeader := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q; filename=%q\r\nContent-Type: application/octet-stream\r\n\r\n",
				multipartBoundary, cfg.MultipartFN, cfg.MultipartFF)
			if _, err := pwUp.Write([]byte(partHeader)); err != nil {
				errCh <- fmt.Errorf("up-writer: %w", err)
				return
			}
		}

		fl := FrameLogger{Prefix: "client-up", LogHeartbeat: cfg.LogHeartbeat}
		var mu sync.Mutex

		if cfg.HeartbeatInterval > 0 {
			go func() {
				tk := time.NewTicker(cfg.HeartbeatInterval)
				defer tk.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tk.C:
						mu.Lock()
						_ = writeFrame(fl, pwUp, FrameHeartbeat, nil)
						mu.Unlock()
					}
				}
			}()
		}

		buf := make([]byte, cfg.BufferSize)
		var paddingBuf []byte
		if cfg.PaddingEnabled && cfg.PaddingSize > 0 {
			paddingBuf = make([]byte, cfg.PaddingSize)
		}

		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				mu.Lock()
				if e := writeFrame(fl, pwUp, FrameData, buf[:n]); e != nil {
					mu.Unlock()
					errCh <- fmt.Errorf("up-writer: write: %w", e)
					return
				}
				if cfg.PaddingEnabled && len(paddingBuf) > 0 {
					_ = writeFrame(fl, pwUp, FramePadding, paddingBuf)
				}
				mu.Unlock()
			}
			if err != nil {
				errCh <- fmt.Errorf("up-writer: tcp: %w", err)
				return
			}
		}
	}()

	// 3) /down reader goroutine
	go func() {
		respDown, err := httpClient.Do(reqDown)
		if err != nil {
			errCh <- fmt.Errorf("down: %w", err)
			return
		}
		defer respDown.Body.Close()

		log.Printf("[client-down] session=%s connected status=%s", sessionID, respDown.Status)

		peek := make([]byte, 5)
		n, err := io.ReadFull(respDown.Body, peek)
		if err != nil {
			errCh <- fmt.Errorf("down: peek failed: %w", err)
			return
		}

		if !isValidFrameType(peek[0]) {
			errCh <- fmt.Errorf("down: invalid stream start 0x%02x", peek[0])
			return
		}

		r := io.MultiReader(&staticReader{b: peek[:n]}, respDown.Body)
		fl := FrameLogger{Prefix: "client-down", LogHeartbeat: cfg.LogHeartbeat}
		for {
			t, payload, err := readFrame(fl, r)
			if err != nil {
				errCh <- fmt.Errorf("down: read: %w", err)
				return
			}
			if t == FrameData {
				if _, err := tcpConn.Write(payload); err != nil {
					errCh <- fmt.Errorf("down: tcp write: %w", err)
					return
				}
			} else if t == FrameClose {
				errCh <- fmt.Errorf("down: server closed")
				return
			}
		}
	}()

	firstErr := <-errCh
	cancel()
	_ = pwUp.Close()
	log.Printf("[client] session=%s closed: %v", sessionID, firstErr)
}

type staticReader struct {
	b []byte
	i int
}

func (sr *staticReader) Read(p []byte) (int, error) {
	if sr.i >= len(sr.b) {
		return 0, io.EOF
	}
	n := copy(p, sr.b[sr.i:])
	sr.i += n
	return n, nil
}
func isValidFrameType(t byte) bool {
	return t >= 0x01 && t <= 0x04
}
