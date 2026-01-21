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
	if v == "" {
		return nil
	}
	h.items = append(h.items, v)
	return nil
}

func (h *headerList) Apply(dst http.Header) error {
	for _, kv := range h.items {
		parts := strings.SplitN(kv, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("bad header %q, must be 'Key: Value'", kv)
		}
		k := http.CanonicalHeaderKey(strings.TrimSpace(parts[0]))
		v := strings.TrimSpace(parts[1])
		if k == "" {
			return fmt.Errorf("bad header %q, empty key", kv)
		}
		dst.Add(k, v)
	}
	return nil
}

type ClientConfig struct {
	ListenAddr string

	UpURL   string
	DownURL string

	SessionHeader string

	HeadersUp   headerList
	HeadersDown headerList

	Multipart   bool
	MultipartFN string // form name
	MultipartFF string // file name

	BufferSize int

	HeartbeatInterval time.Duration

	PaddingEnabled bool
	PaddingSize    int

	HTTPTimeout time.Duration

	LogHeartbeat bool
}

type ServerConfig struct {
	HTTPListenAddr string
	UpPath         string
	DownPath       string

	SessionHeader string

	TargetTCP string

	BufferSize int

	HeartbeatInterval time.Duration

	PaddingEnabled bool
	PaddingSize    int

	// multipart settings: server /up can parse multipart and pick part name
	AcceptMultipart   bool
	MultipartFormName string

	WaitPeerTimeout time.Duration
}

func parseClientFlags(args []string) (*ClientConfig, error) {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)

	cfg := &ClientConfig{}

	fs.StringVar(&cfg.ListenAddr, "listen", ":6000", "local TCP listen addr (accept inbound TCP and tunnel to HTTP)")
	fs.StringVar(&cfg.UpURL, "up", "", "HTTP URL for /up (required)")
	fs.StringVar(&cfg.DownURL, "down", "", "HTTP URL for /down (required)")
	fs.StringVar(&cfg.SessionHeader, "session-header", "X-Session-ID", "header key used for session id")

	fs.Var(&cfg.HeadersUp, "H-up", "extra header for up request, repeatable: 'Key: Value'")
	fs.Var(&cfg.HeadersDown, "H-down", "extra header for down request, repeatable: 'Key: Value'")

	fs.BoolVar(&cfg.Multipart, "multipart", true, "send /up as multipart/form-data (stream in file part)")
	fs.StringVar(&cfg.MultipartFN, "multipart-form", "file", "multipart form field name (when -multipart=true)")
	fs.StringVar(&cfg.MultipartFF, "multipart-file", "upload.bin", "multipart filename (when -multipart=true)")

	fs.IntVar(&cfg.BufferSize, "buf", 4096, "TCP read/write buffer size")
	fs.DurationVar(&cfg.HeartbeatInterval, "hb", 100*time.Millisecond, "heartbeat interval (0 to disable)")
	fs.BoolVar(&cfg.PaddingEnabled, "padding", true, "append padding frames after each data frame")
	fs.IntVar(&cfg.PaddingSize, "padding-size", 4096, "padding payload size (bytes)")

	fs.DurationVar(&cfg.HTTPTimeout, "http-timeout", 0, "HTTP client timeout (0 means no timeout)")
	fs.BoolVar(&cfg.LogHeartbeat, "log-heartbeat", false, "log heartbeat frames (very noisy)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if cfg.UpURL == "" || cfg.DownURL == "" {
		return nil, errors.New("client requires -up and -down")
	}
	if cfg.BufferSize <= 0 {
		return nil, errors.New("-buf must be > 0")
	}
	if cfg.PaddingEnabled && cfg.PaddingSize < 0 {
		return nil, errors.New("-padding-size must be >= 0")
	}
	if cfg.MultipartFN == "" {
		cfg.MultipartFN = "file"
	}
	if cfg.MultipartFF == "" {
		cfg.MultipartFF = "upload.bin"
	}
	if strings.TrimSpace(cfg.SessionHeader) == "" {
		return nil, errors.New("-session-header cannot be empty")
	}

	return cfg, nil
}

func parseServerFlags(args []string) (*ServerConfig, error) {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)

	cfg := &ServerConfig{}

	fs.StringVar(&cfg.HTTPListenAddr, "listen", ":18080", "HTTP listen addr")
	fs.StringVar(&cfg.UpPath, "up-path", "/up", "HTTP path for up")
	fs.StringVar(&cfg.DownPath, "down-path", "/down", "HTTP path for down")

	fs.StringVar(&cfg.SessionHeader, "session-header", "X-Session-ID", "header key used for session id")
	fs.StringVar(&cfg.TargetTCP, "target", "127.0.0.1:25022", "target TCP address to dial for each session")

	fs.IntVar(&cfg.BufferSize, "buf", 4096, "TCP read/write buffer size")
	fs.DurationVar(&cfg.HeartbeatInterval, "hb", 100*time.Millisecond, "heartbeat interval (0 to disable)")
	fs.BoolVar(&cfg.PaddingEnabled, "padding", true, "send padding frames after each data frame")
	fs.IntVar(&cfg.PaddingSize, "padding-size", 4096, "padding payload size (bytes)")

	fs.BoolVar(&cfg.AcceptMultipart, "accept-multipart", true, "accept multipart/form-data on /up and stream from file part")
	fs.StringVar(&cfg.MultipartFormName, "multipart-form", "file", "multipart form field name to select (when -accept-multipart=true)")

	fs.DurationVar(&cfg.WaitPeerTimeout, "wait-peer-timeout", 30*time.Second, "max wait for the other half (/up or /down) before failing")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if strings.TrimSpace(cfg.SessionHeader) == "" {
		return nil, errors.New("-session-header cannot be empty")
	}
	if cfg.TargetTCP == "" {
		return nil, errors.New("-target required")
	}
	if cfg.BufferSize <= 0 {
		return nil, errors.New("-buf must be > 0")
	}
	if cfg.PaddingEnabled && cfg.PaddingSize < 0 {
		return nil, errors.New("-padding-size must be >= 0")
	}
	if cfg.UpPath == "" || cfg.DownPath == "" {
		return nil, errors.New("-up-path and -down-path required")
	}
	if !strings.HasPrefix(cfg.UpPath, "/") {
		cfg.UpPath = "/" + cfg.UpPath
	}
	if !strings.HasPrefix(cfg.DownPath, "/") {
		cfg.DownPath = "/" + cfg.DownPath
	}
	if cfg.MultipartFormName == "" {
		cfg.MultipartFormName = "file"
	}
	if cfg.WaitPeerTimeout <= 0 {
		cfg.WaitPeerTimeout = 30 * time.Second
	}

	return cfg, nil
}
