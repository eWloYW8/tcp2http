package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type headerList struct {
	items []string
}

func (h *headerList) String() string { return strings.Join(h.items, "; ") }
func (h *headerList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v != "" {
		h.items = append(h.items, v)
	}
	return nil
}

func (h *headerList) Apply(dst http.Header) error {
	for _, kv := range h.items {
		parts := strings.SplitN(kv, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("bad header %q", kv)
		}
		dst.Add(http.CanonicalHeaderKey(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1]))
	}
	return nil
}

type ClientConfig struct {
	ListenAddr               string
	UpURL, DownURL           string
	SessionHeader            string
	HeadersUp, HeadersDown   headerList
	Multipart                bool
	MultipartFN, MultipartFF string
	BufferSize               int
	HeartbeatInterval        time.Duration
	PaddingEnabled           bool
	PaddingSize              int
	HTTPTimeout              time.Duration
	LogHeartbeat             bool
}

type ServerConfig struct {
	HTTPListenAddr    string
	UpPath, DownPath  string
	SessionHeader     string
	TargetTCP         string
	BufferSize        int
	HeartbeatInterval time.Duration
	PaddingEnabled    bool
	PaddingSize       int
	AcceptMultipart   bool
	MultipartFormName string
	WaitPeerTimeout   time.Duration
}

func parseClientFlags(args []string) (*ClientConfig, error) {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	cfg := &ClientConfig{}
	fs.StringVar(&cfg.ListenAddr, "listen", ":6000", "")
	fs.StringVar(&cfg.UpURL, "up", "", "")
	fs.StringVar(&cfg.DownURL, "down", "", "")
	fs.StringVar(&cfg.SessionHeader, "session-header", "X-Session-ID", "")
	fs.Var(&cfg.HeadersUp, "H-up", "")
	fs.Var(&cfg.HeadersDown, "H-down", "")
	fs.BoolVar(&cfg.Multipart, "multipart", true, "")
	fs.StringVar(&cfg.MultipartFN, "multipart-form", "file", "")
	fs.StringVar(&cfg.MultipartFF, "multipart-file", "upload.bin", "")
	fs.IntVar(&cfg.BufferSize, "buf", 4096, "")
	fs.DurationVar(&cfg.HeartbeatInterval, "hb", 100*time.Millisecond, "")
	fs.BoolVar(&cfg.PaddingEnabled, "padding", true, "")
	fs.IntVar(&cfg.PaddingSize, "padding-size", 4096, "")
	fs.DurationVar(&cfg.HTTPTimeout, "http-timeout", 0, "")
	fs.BoolVar(&cfg.LogHeartbeat, "log-heartbeat", false, "")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if cfg.UpURL == "" || cfg.DownURL == "" {
		return nil, errors.New("missing urls")
	}
	return cfg, nil
}

func parseServerFlags(args []string) (*ServerConfig, error) {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	cfg := &ServerConfig{}
	fs.StringVar(&cfg.HTTPListenAddr, "listen", ":18080", "")
	fs.StringVar(&cfg.UpPath, "up-path", "/up", "")
	fs.StringVar(&cfg.DownPath, "down-path", "/down", "")
	fs.StringVar(&cfg.SessionHeader, "session-header", "X-Session-ID", "")
	fs.StringVar(&cfg.TargetTCP, "target", "127.0.0.1:22", "")
	fs.IntVar(&cfg.BufferSize, "buf", 4096, "")
	fs.DurationVar(&cfg.HeartbeatInterval, "hb", 100*time.Millisecond, "")
	fs.BoolVar(&cfg.PaddingEnabled, "padding", true, "")
	fs.IntVar(&cfg.PaddingSize, "padding-size", 4096, "")
	fs.BoolVar(&cfg.AcceptMultipart, "accept-multipart", true, "")
	fs.StringVar(&cfg.MultipartFormName, "multipart-form", "file", "")
	fs.DurationVar(&cfg.WaitPeerTimeout, "wait-peer-timeout", 30*time.Second, "")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return cfg, nil
}
