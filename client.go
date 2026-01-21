package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

func runClient(args []string) error {
	cfg, err := parseClientFlags(args)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Printf("[client] listening on %s", cfg.ListenAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleClientConn(cfg, conn)
	}
}

func handleClientConn(cfg *ClientConfig, tcpConn net.Conn) {
	defer tcpConn.Close()
	sessionID := newSessionID()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	swUp := NewSafeWriter(pw)
	httpClient := &http.Client{}
	if cfg.HTTPTimeout > 0 {
		httpClient.Timeout = cfg.HTTPTimeout
	}

	boundary := ""
	if cfg.Multipart {
		boundary = newBoundary()
	}

	reqUp, _ := http.NewRequestWithContext(ctx, "POST", cfg.UpURL, pr)
	reqUp.Header.Set(cfg.SessionHeader, sessionID)
	reqUp.ContentLength = -1
	if cfg.Multipart {
		reqUp.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	} else {
		reqUp.Header.Set("Content-Type", "application/octet-stream")
	}
	_ = cfg.HeadersUp.Apply(reqUp.Header)

	errCh := make(chan error, 2)

	go func() {
		resp, err := httpClient.Do(reqUp)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		errCh <- fmt.Errorf("up-http-closed")
	}()

	go func() {
		fl := FrameLogger{Prefix: "client-up", LogHeartbeat: cfg.LogHeartbeat}
		if cfg.Multipart {
			h := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=%q; filename=%q\r\nContent-Type: application/octet-stream\r\n\r\n",
				boundary, cfg.MultipartFN, cfg.MultipartFF)
			_, _ = pw.Write([]byte(h))
		}

		buf := make([]byte, cfg.BufferSize)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				swUp.SetPriority(true)
				if e := swUp.WriteFrame(fl, FrameData, buf[:n]); e != nil {
					swUp.SetPriority(false)
					break
				}
				if cfg.PaddingMode == PaddingPerPacket {
					_ = swUp.WriteFrame(fl, FramePadding, make([]byte, cfg.PaddingSize))
				}
				swUp.SetPriority(false)
			}
			if err != nil {
				break
			}
		}

		_ = swUp.WriteFrame(fl, FrameClose, nil)
		if cfg.Multipart {
			_, _ = pw.Write([]byte("\r\n--" + boundary + "--\r\n"))
		}
		pw.Close()
		cancel()
	}()

	go func() {
		reqDown, _ := http.NewRequestWithContext(ctx, "POST", cfg.DownURL, nil)
		reqDown.Header.Set(cfg.SessionHeader, sessionID)
		_ = cfg.HeadersDown.Apply(reqDown.Header)
		resp, err := httpClient.Do(reqDown)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()

		fl := FrameLogger{Prefix: "client-down", LogHeartbeat: cfg.LogHeartbeat}
		for {
			t, payload, err := readFrame(fl, resp.Body)
			if err != nil {
				break
			}
			if t == FrameData {
				if _, err := tcpConn.Write(payload); err != nil {
					break
				}
			} else if t == FrameClose {
				break
			}
		}
		errCh <- fmt.Errorf("down-closed")
	}()

	flUp := FrameLogger{Prefix: "client-up", LogHeartbeat: cfg.LogHeartbeat}
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
			if err := swUp.WriteFrame(flUp, FrameHeartbeat, nil); err != nil {
				goto DONE
			}
		case <-padTicker.C:
			if swUp.CheckAndResetDataSent() {
				if _, err := swUp.TryWriteFrame(flUp, FramePadding, make([]byte, cfg.PaddingSize)); err != nil {
					goto DONE
				}
			}
		case <-ctx.Done():
			goto DONE
		case err := <-errCh:
			log.Printf("[client] session=%s error: %v", sessionID, err)
			return
		}
	}

DONE:
	log.Printf("[client] session=%s closing", sessionID)
}
