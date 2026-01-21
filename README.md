# tcp2http

A lightweight utility to tunnel TCP connections over full-duplex WebSocket-free HTTP 1.1 POST streams.
The client keeps a single HTTP tunnel to the server and multiplexes multiple TCP connections as channels on that tunnel.

## Build

[![Build and Release](https://github.com/eWloYW8/tcp2http/actions/workflows/build.yml/badge.svg)](https://github.com/eWloYW8/tcp2http/actions/workflows/build.yml)


```bash
go build -o tcp2http .
```

## Quick start

Start the server:

```bash
./tcp2http server \
  -listen :18080 \
  -target 127.0.0.1:22
```

Start the client:

```bash
./tcp2http client \
  -listen :6000 \
  -up http://server:18080/up \
  -down http://server:18080/down
```

Then connect any TCP client to `127.0.0.1:6000`. Each new TCP connection becomes a new channel inside the shared HTTP tunnel.

## Client flags

- `-listen` (default `:6000`): local TCP listen address.
- `-up` (required): HTTP upload URL, e.g. `http://server:18080/up`.
- `-down` (required): HTTP download URL, e.g. `http://server:18080/down`.
- `-session-header` (default `X-Session-ID`): header used to correlate the up/down streams.
- `-H-up` (repeatable): add HTTP header to the upload request, format `Key: Value`.
- `-H-down` (repeatable): add HTTP header to the download request, format `Key: Value`.
- `-multipart` (default `true`): wrap the upload stream in multipart/form-data.
- `-multipart-form` (default `file`): multipart form field name.
- `-multipart-file` (default `upload.bin`): multipart file name.
- `-buf` (default `4096`): buffer size for TCP reads.
- `-hb` (default `100ms`): heartbeat interval on the HTTP tunnel; `0` disables.
- `-padding-mode` (default `none`): `none`, `per-packet`, or `interval`.
- `-padding-interval` (default `500ms`): padding interval when `padding-mode=interval`.
- `-padding-size` (default `4096`): padding payload size.
- `-http-timeout` (default `0`): HTTP client timeout; `0` means no timeout.
- `-log-heartbeat` (default `false`): include heartbeat frames in logs.
- `-log-level` (default `info`): `debug`, `info`, `warn`, `error`, or `off`.

## Server flags

- `-listen` (default `:18080`): HTTP listen address.
- `-up-path` (default `/up`): upload path.
- `-down-path` (default `/down`): download path.
- `-session-header` (default `X-Session-ID`): header used to correlate the up/down streams.
- `-target` (default `127.0.0.1:22`): target TCP address for each channel.
- `-buf` (default `4096`): buffer size for TCP reads.
- `-hb` (default `100ms`): heartbeat interval on the HTTP tunnel; `0` disables.
- `-padding-mode` (default `none`): `none`, `per-packet`, or `interval`.
- `-padding-interval` (default `500ms`): padding interval when `padding-mode=interval`.
- `-padding-size` (default `4096`): padding payload size.
- `-accept-multipart` (default `true`): accept multipart/form-data uploads.
- `-multipart-form` (default `file`): multipart form field name to read.
- `-wait-peer-timeout` (default `30s`): wait time for peer stream to connect.
- `-log-level` (default `info`): `debug`, `info`, `warn`, `error`, or `off`.

## Examples

### Tunneling SSH over ZJU WebVPN

ZJU WebVPN only allows HTTP/HTTPS traffic, so you can use `tcp2http` to tunnel your SSH or other TCP connections over it:

Server (inside ZJU Campus Network):

```bash
./tcp2http server \
  -listen :18080 \
  -target 127.0.0.1:1022 \
  -up-path /up \
  -down-path /down \
  -buf 8192 \
  -hb 100ms \
  -padding-mode=interval -padding-interval 10ms -padding-size 4096
```

We enable padding to avoid the impact of the 4KB download buffer in the webvpn web server.

You can try different padding settings to find the best performance.

Client:

```bash
./tcp2http client \                     
  -listen :6000 \
  -up "https://webvpn.zju.edu.cn/http-18080/{{Server URL encrypted by WebVPN}}/up" \
  -down "https://webvpn.zju.edu.cn/http-18080/{{Server URL encrypted by WebVPN}}/down" \
  -H-up "Cookie: wengine_vpn_ticketwebvpn_zju_edu_cn={{Your WebVPN Cookie Here}}" \
  -H-down "Cookie: wengine_vpn_ticketwebvpn_zju_edu_cn={{Your WebVPN Cookie Here}}" \
  -buf 8192 \
  -multipart=true \
  -hb 100ms \
  -padding-mode=none
```

We disable padding on the client side since the webvpn server does not have upload buffer.

Then you can connect to `localhost:6000` in client side to access your SSH server inside ZJU Campus Network.

For encryption of the server URL and obtaining WebVPN cookies, please refer to [eWloYW8/ZJUWebVPN](https://github.com/eWloYW8/ZJUWebVPN).