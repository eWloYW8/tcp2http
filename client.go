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

	// -------- /up: streaming request body via Pipe --------
	prUp, pwUp := io.Pipe()

	var multipartBoundary string
	if cfg.Multipart {
		multipartBoundary = newBoundary()
	}

	reqUp, err := http.NewRequestWithContext(ctx, "POST", cfg.UpURL, prUp)
	if err != nil {
		log.Printf("[client] session=%s build up request error: %v", sessionID, err)
		return
	}
	reqUp.Header.Set(cfg.SessionHeader, sessionID)
	reqUp.TransferEncoding = []string{"chunked"}

	if cfg.Multipart {
		reqUp.Header.Set("Content-Type", "multipart/form-data; boundary="+multipartBoundary)
	} else {
		reqUp.Header.Set("Content-Type", "application/octet-stream")
	}
	if err := cfg.HeadersUp.Apply(reqUp.Header); err != nil {
		log.Printf("[client] session=%s apply up headers error: %v", sessionID, err)
		return
	}

	// -------- /down: request --------
	reqDown, err := http.NewRequestWithContext(ctx, "POST", cfg.DownURL, nil)
	if err != nil {
		log.Printf("[client] session=%s build down request error: %v", sessionID, err)
		return
	}
	reqDown.Header.Set(cfg.SessionHeader, sessionID)
	reqDown.Header.Set("Content-Length", "0")
	if err := cfg.HeadersDown.Apply(reqDown.Header); err != nil {
		log.Printf("[client] session=%s apply down headers error: %v", sessionID, err)
		return
	}

	// 收敛：任何一边失败都 cancel，另一边退出
	errCh := make(chan error, 3)

	// 1) /up http goroutine
	go func() {
		resp, err := httpClient.Do(reqUp)
		if err != nil {
			errCh <- fmt.Errorf("up-http: %w", err)
			return
		}
		log.Printf("[client-up-http] session=%s status=%s", sessionID, resp.Status)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		errCh <- fmt.Errorf("up-http: closed")
	}()

	// 2) /up writer goroutine (tcp -> frame -> pipe)
	go func() {
		defer func() {
			_ = writeFrame(FrameLogger{Prefix: "client-up", LogHeartbeat: cfg.LogHeartbeat}, pwUp, FrameClose, nil)
			if cfg.Multipart {
				end := fmt.Sprintf("\r\n--%s--\r\n", multipartBoundary)
				_, _ = pwUp.Write([]byte(end))
			}
			_ = pwUp.Close()
		}()

		// multipart part header
		if cfg.Multipart {
			partHeader := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q; filename=%q\r\nContent-Type: application/octet-stream\r\n\r\n",
				multipartBoundary, cfg.MultipartFN, cfg.MultipartFF)
			if _, err := pwUp.Write([]byte(partHeader)); err != nil {
				errCh <- fmt.Errorf("up-writer: write multipart header: %w", err)
				return
			}
		}

		fl := FrameLogger{Prefix: "client-up", LogHeartbeat: cfg.LogHeartbeat}

		// Pipe 写入串行化：heartbeat + data
		var mu sync.Mutex

		// heartbeat
		var hbDone = make(chan struct{})
		if cfg.HeartbeatInterval > 0 {
			go func() {
				tk := time.NewTicker(cfg.HeartbeatInterval)
				defer tk.Stop()
				for {
					select {
					case <-ctx.Done():
						close(hbDone)
						return
					case <-tk.C:
						mu.Lock()
						_ = writeFrame(fl, pwUp, FrameHeartbeat, nil)
						mu.Unlock()
					}
				}
			}()
		} else {
			close(hbDone)
		}

		buf := make([]byte, cfg.BufferSize)
		var paddingBuf []byte
		if cfg.PaddingEnabled && cfg.PaddingSize > 0 {
			paddingBuf = make([]byte, cfg.PaddingSize)
		}

		for {
			select {
			case <-ctx.Done():
				<-hbDone
				errCh <- fmt.Errorf("up-writer: ctx done")
				return
			default:
			}

			n, err := tcpConn.Read(buf)
			if n > 0 {
				mu.Lock()
				if e := writeFrame(fl, pwUp, FrameData, buf[:n]); e != nil {
					mu.Unlock()
					<-hbDone
					errCh <- fmt.Errorf("up-writer: write data: %w", e)
					return
				}
				if cfg.PaddingEnabled && len(paddingBuf) > 0 {
					_ = writeFrame(fl, pwUp, FramePadding, paddingBuf)
				}
				mu.Unlock()
			}
			if err != nil {
				<-hbDone
				errCh <- fmt.Errorf("up-writer: tcp read: %w", err)
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
		defer func() {
			_ = respDown.Body.Close()
		}()

		log.Printf("[client-down] session=%s connected status=%s", sessionID, respDown.Status)

		// 先读前 256 字节做探测：如果不是 frame 流，直接打印出来（避免把 HTML 当 frame）
		peek := make([]byte, 256)
		n, _ := io.ReadAtLeast(respDown.Body, peek, 5)
		peek = peek[:n]

		// 拼回去继续读
		r := io.MultiReader(bytesReader(peek), respDown.Body)

		if len(peek) > 0 && !isValidFrameType(peek[0]) {
			log.Printf("[client-down] session=%s NOT framed stream. firstByte=0x%02x.",
				sessionID, peek[0])
			errCh <- fmt.Errorf("down: not framed stream (likely HTML/login/error page)")
			return
		}

		fl := FrameLogger{Prefix: "client-down", LogHeartbeat: cfg.LogHeartbeat}
		for {
			t, payload, err := readFrame(fl, r)
			if err != nil {
				errCh <- fmt.Errorf("down: readFrame: %w", err)
				return
			}
			switch t {
			case FrameData:
				if _, err := tcpConn.Write(payload); err != nil {
					errCh <- fmt.Errorf("down: write tcp: %w", err)
					return
				}
			case FramePadding:
			case FrameHeartbeat:
			case FrameClose:
				errCh <- fmt.Errorf("down: close frame")
				return
			default:
				// ignore unknown
			}
		}
	}()

	// 任何一个 goroutine 报错/结束，统一收敛关闭
	firstErr := <-errCh
	cancel()
	_ = pwUp.Close()
	_ = tcpConn.Close()

	log.Printf("[client] session=%s closed (%v)", sessionID, firstErr)
}

type staticReader struct {
	b []byte
	i int
}

func bytesReader(b []byte) io.Reader { return &staticReader{b: b} }

func (sr *staticReader) Read(p []byte) (int, error) {
	if sr.i >= len(sr.b) {
		return 0, io.EOF
	}
	n := copy(p, sr.b[sr.i:])
	sr.i += n
	return n, nil
}

func isValidFrameType(t byte) bool {
	switch t {
	case FrameData, FrameHeartbeat, FrameClose, FramePadding:
		return true
	default:
		return false
	}
}
