# camera_rpi — Design Document (v0.1, for review)

A small Go application that runs on a Raspberry Pi 4 with the HQ Camera
(Sony IMX477) and serves a web UI that:

1. shows the live camera stream in the browser with **minimal latency**,
   usable both on the LAN and **over the internet** at HQ (1080p) with
   reasonable bandwidth;
2. lets the user **change camera parameters** (resolution, frame rate,
   exposure, gain, white balance, image tuning, orientation, zoom, bitrate…);
3. lets the user take a **full-resolution still** (up to 4056×3040, JPEG/PNG
   plus optional DNG raw) and download it.

It is a prototype. Main goals: **simple**, **few dependencies**, **easy to
replace parts later**.

---

## 1. Constraints and assumptions

| Item | Assumption |
|---|---|
| Hardware | Raspberry Pi 4 (any RAM), HQ Camera (IMX477) on CSI, C/CS lens (manual focus, no AF) |
| OS | Raspberry Pi OS Bookworm or newer (libcamera stack), 64-bit preferred |
| Camera tools | `rpicam-apps` (`rpicam-vid`, `rpicam-still`, `rpicam-hello`), installed by default. On older Bullseye they are called `libcamera-*`; the command prefix is configurable |
| Go | 1.22+ (needed for the method and path patterns in `net/http.ServeMux`) |
| Go dependencies | **None, stdlib only** (`net/http`, `html/template`, `embed`, `os/exec`, `crypto/tls`, …) |
| Frontend | Plain HTML from Go templates plus vanilla JS. **No JS libraries.** Video decoding uses the browser's built-in WebCodecs API |
| Browsers | Current Chrome / Edge / Firefox (130+) / Safari (16.4+), desktop and mobile |
| Cameras | One camera per Pi. The camera is opened by **one process at a time** |

### Pi 4 hardware facts that drive the design
- Pi 4 has a **hardware H.264 encoder** (V4L2 M2M, level 4.2, max 1920×1080,
  1080p30 reliable, 1080p50 near the limit). It also has a hardware **MJPEG**
  encoder. `rpicam-vid` uses both. (The Pi 5 has no H.264 hardware encoder; this
  design targets the Pi 4.)
- HQ camera sensor modes (libcamera picks one automatically; the user can force
  one):

  | Mode | Max fps | Notes |
  |---|---|---|
  | 1332×990 (10-bit, binned and cropped) | 120 | small FOV, fast |
  | 2028×1080 (12-bit, binned and cropped) | 50 | 16:9, good for 1080p |
  | 2028×1520 (12-bit, 2×2 binned) | 40 | full FOV 4:3 |
  | 4056×3040 (12-bit, full) | 10 | stills |

- The camera cannot be shared between processes. So taking a full-res still
  means **briefly stopping the video pipeline** (see §6).

---

## 2. High-level architecture

```
                         Raspberry Pi 4
┌──────────────────────────────────────────────────────────────────────────┐
│                                                                          │
│  ┌───────────────┐ stdout: H.264 Annex-B  ┌──────────────┐               │
│  │  rpicam-vid   │───────────────────────▶│ h264 splitter │              │
│  │ (subprocess)  │                        │ (NAL → AU)    │              │
│  └───────▲───────┘                        └──────┬───────┘               │
│          │ start/stop with args                  │ AccessUnit{data,key,ts}│
│  ┌───────┴──────────────────────────┐     ┌──────▼───────┐               │
│  │ camera.Manager (single goroutine)│     │  stream.Hub  │ fan-out,      │
│  │  state machine, owns the camera  │     │  per-client  │ drop-to-key   │
│  │  - ApplyConfig → restart vid     │     │  queues      │               │
│  │  - CaptureStill → stop vid,      │     └──────┬───────┘               │
│  │    run rpicam-still, restart vid │            │                       │
│  └───────▲──────────────▲───────────┘            │                       │
│          │              │                        │                       │
│  ┌───────┴──────────────┴────────────────────────▼───────────────────┐   │
│  │                       web.Server (net/http)                        │   │
│  │  GET /            index (html/template, embedded)                  │   │
│  │  GET /static/*    app.js, player.js, style.css (embedded)          │   │
│  │  GET /stream      long-lived HTTP response, framed H.264           │   │
│  │  GET/PUT /api/config, GET /api/capabilities, GET /api/status       │   │
│  │  POST /api/capture, GET/DELETE /captures/{name}, GET /api/captures │   │
│  └───────────────────────────────▲───────────────────────────────────┘   │
└──────────────────────────────────┼───────────────────────────────────────┘
                                   │ HTTPS (HTTP/1.1 or HTTP/2)
┌──────────────────────────────────┼───────────────────────────────────────┐
│ Browser                          │                                       │
│  fetch('/stream') ─▶ ReadableStream ─▶ frame parser ─▶ VideoDecoder      │
│                                          (WebCodecs, HW) ─▶ <canvas>     │
│  settings form ─▶ fetch PUT /api/config                                  │
│  capture button ─▶ fetch POST /api/capture ─▶ gallery                    │
└──────────────────────────────────────────────────────────────────────────┘
```

Key ideas:
- **Do not talk to libcamera from Go.** Run `rpicam-vid` / `rpicam-still` as
  child processes and read the encoded stream from stdout. No cgo, no C++
  bindings, cross-compiles to one static binary.
- **Encode once, fan out to N viewers.** All viewers get the same H.264
  stream. The Pi never transcodes.
- **Transport = one plain HTTP response streamed with `fetch()`**, carrying
  length-prefixed H.264 access units. **Decode = WebCodecs `VideoDecoder`**
  (hardware accelerated) and draw to a `<canvas>`. No WebSocket library, no
  WebRTC stack, no MSE/fMP4 muxing.

---

## 3. Choosing the streaming protocol

Requirements: low latency (target <150 ms glass-to-glass on LAN), HQ (1080p)
at internet-friendly bitrates (2–8 Mbps), simple, few dependencies.

| Option | Latency | Bandwidth @1080p30 | Server deps / complexity | Browser side | Verdict |
|---|---|---|---|---|---|
| **MJPEG over HTTP multipart** | very low (~100 ms) | **20–50 Mbps** | stdlib, trivial | `<img>` tag, nothing else | Great on LAN, unusable over most internet links. **Fallback option only** |
| HLS / DASH | 2–10 s | good | moderate (segmenter) | native/hls.js | Too slow |
| LL-HLS | 1–3 s | good | high | hls.js | Too slow, complex |
| MSE + fragmented MP4 | 0.3–1 s (MSE buffers, needs tricks) | good | hand-written fMP4 muxer (~300 LOC) or JS lib (jmuxer) | MSE | Works without HTTPS, but latency is higher and less predictable |
| **WebRTC** (pion) | lowest (~100 ms), UDP, congestion control | good, adaptive | **pion: large dependency tree**, signalling, ICE, STUN/TURN for NAT | native | Best for bad internet links but heavy. **Upgrade path**, not v1 |
| H.264 over WebSocket + WebCodecs | ~100–150 ms | good | needs a WebSocket lib (gorilla / x/net) | WebCodecs | Good, but adds a dependency for a channel that only goes one way |
| **H.264 over streamed HTTP (`fetch` + ReadableStream) + WebCodecs** | ~100–150 ms | good (2–8 Mbps) | **stdlib only** (`http.Flusher`) | WebCodecs, no libs | **Chosen** |

### Why streamed HTTP + WebCodecs
- **Zero dependencies** on both sides. The server writes length-prefixed frames
  to an `http.ResponseWriter` and flushes. The browser reads
  `response.body.getReader()`.
- **Same bandwidth as any H.264 solution**: 1080p30 looks good at 4–8 Mbps,
  720p30 at 2–3 Mbps.
- **Low latency**: no container, no player buffer. Each frame is decoded as soon
  as it arrives and drawn on the next animation frame.
- **Internet friendly**: it is just HTTPS on one port. It works through reverse
  proxies (Caddy, nginx with `proxy_buffering off`), Cloudflare Tunnel,
  Tailscale and so on. Go serves HTTP/2 over TLS for free.
- Control is ordinary REST (`fetch` with JSON), so we need no bidirectional
  channel.

### Known trade-offs (please review)
1. **WebCodecs needs a secure context** (HTTPS or `http://localhost`). Opening
   `http://192.168.1.50:8080` on the LAN will **not** give you WebCodecs. Ways
   around it:
   - built-in TLS with an **auto-generated self-signed certificate** (stdlib
     `crypto/x509`, made on first start, the browser shows a one-time warning);
   - or your own cert/key (`-tls-cert`, `-tls-key`);
   - or a reverse proxy or tunnel that terminates TLS (recommended for internet
     use: Caddy, Cloudflare Tunnel, Tailscale Serve);
   - or the **MJPEG fallback mode** (§5.6), which works over plain HTTP.
2. **TCP head-of-line blocking** on lossy links: a lost packet stalls the stream
   for one retransmit. This is fine on normal broadband and LTE, worse on very
   lossy links. We stop latency from piling up by **dropping frames on the
   server until the next keyframe** (§5.4). If that is not good enough over the
   internet, the next step is WebRTC. Only the transport layer changes; the
   camera, splitter and hub layers stay the same.
3. One encoder means **one bitrate/resolution for all viewers** (no per-viewer
   adaptive bitrate). The user picks the bitrate in the UI. This is fine for a
   prototype.

---

## 4. Camera layer (`internal/camera`)

### 4.1 Video pipeline (`rpicam-vid`)

The command line is built from `Config`:

```
rpicam-vid -n -t 0 \
  --codec h264 --inline --flush \
  --profile high --level 4.2 \
  --width 1920 --height 1080 --framerate 30 \
  --bitrate 6000000 --intra 30 \
  [--mode 2028:1080:12:P] \
  [--shutter 10000] [--gain 2.0] [--ev 0] \
  [--exposure normal|sport|long] [--metering centre|spot|average] \
  [--awb auto|incandescent|tungsten|fluorescent|indoor|daylight|cloudy] [--awbgains 1.5,1.2] \
  [--brightness 0] [--contrast 1] [--saturation 1] [--sharpness 1] \
  [--denoise auto|off|cdn_off|cdn_fast|cdn_hq] \
  [--hflip] [--vflip] [--rotation 0|180] \
  [--roi x,y,w,h] \
  -o -
```

- `-o -` sends the stream to stdout. `--inline` repeats SPS/PPS before every
  IDR, so any viewer can start at any keyframe. `--flush` flushes stdout after
  every frame. `-n` means no preview window. `-t 0` means run forever.
- `--intra 30` puts a keyframe every 1 s at 30 fps. This limits (a) how long a
  new viewer waits for the first picture and (b) how long it takes to recover
  after frames are dropped. It is configurable (the trade-off is bitrate spikes
  from I-frames).
- The Pi encoder produces no B-frames, so decode order equals display order.
  This keeps the decoder simple.
- `stderr` is captured and logged (line by line, with a prefix) for diagnostics.

**Applying a new config restarts `rpicam-vid`.** `rpicam-vid` cannot change
controls while it runs. A restart takes about 0.5–1 s, during which viewers see
the last frame and a "reconfiguring…" overlay. That is acceptable for a
prototype. (The way to live changes for exposure, gain and AWB later is a
small C++ helper that links libcamera directly, like mediamtx does. Out of
scope.)

### 4.2 Still capture (`rpicam-still`)

```
rpicam-still -n -t 1000 [--immediate] \
  --width 4056 --height 3040 -e jpg -q 95 [--raw] \
  <same exposure / awb / image / orientation flags as video> \
  -o <capturesDir>/IMG_20261106_134501.jpg
```

- With **auto exposure**, `-t 1000` lets AE/AWB settle for about 1 s
  (configurable).
- With **manual shutter and gain**, we pass `--immediate` and skip the
  settling time. The capture is then almost instant and matches the preview.
- `--raw` also writes a `.dng` (12-bit raw) next to the JPEG. Optional, set in
  the UI.
- Formats: `jpg` (default), `png`, plus optional DNG.

### 4.3 Manager: one goroutine owns the camera

All camera operations go through one goroutine that receives commands on a
channel. That serialises access to the camera and avoids races between
"apply config", "capture still" and process crashes.

```
          ┌───────────┐ start ok   ┌───────────┐
  start ─▶│ Starting  │──────────▶│ Streaming │◀───────────────┐
          └─────▲─────┘            └─┬───┬───┬─┘                │
                │                    │   │   │ process exited   │
                │      ApplyConfig   │   │   └──────────┐       │
                │   ┌────────────────┘   │ CaptureStill │       │
                │   ▼                    ▼              ▼       │
          ┌─────┴──────┐        ┌────────────┐   ┌───────────┐  │
          │ Restarting │        │ Capturing  │   │  Backoff  │──┘
          └────────────┘        │ (still)    │   │ 1s,2s..30s│ retry
                                └─────┬──────┘   └───────────┘
                                      │ done → restart video
                                      └──▶ Starting
```

Commands:
- `ApplyConfig(cfg) error`: validate, stop the video process (SIGINT, wait up
  to 2 s, then SIGKILL), start it with the new args, save the config to disk.
- `CaptureStill(params) (CaptureResult, error)`: stop video, run
  `rpicam-still`, start video again. The video comes back even if the still
  fails.
- `Status() Status`: state, current config, uptime, process restarts, last
  error.

Each state change is published to the Hub as an **event** (`streaming`,
`reconfiguring`, `capturing`, `error`), so the UI can show an overlay.

### 4.4 Capabilities discovery
At startup we run `rpicam-hello --list-cameras` once and parse sensor modes
(size, bit depth, max fps). `/api/capabilities` exposes them so the UI lists
real modes, not hard-coded ones. If parsing fails, we use the built-in IMX477
table from §1.

### 4.5 Config model

```go
type Config struct {
    // Stream
    Width, Height int     // e.g. 1920x1080; validated <= 1920x1080 for H.264 on Pi4
    Framerate     float64 // 1..120 (limited by sensor mode)
    Bitrate       int     // bps, 500k..25M
    Intra         int     // GOP length in frames
    Profile       string  // baseline|main|high
    SensorMode    string  // "" = auto, or "2028:1080:12:P"
    Codec         string  // "h264" (default) | "mjpeg" (fallback mode, §5.6)
    MJPEGQuality  int

    // Exposure
    Shutter   int     // µs, 0 = auto
    Gain      float64 // analogue gain, 0 = auto
    EV        float64 // -10..10
    Exposure  string  // normal|sport|long
    Metering  string  // centre|spot|average

    // White balance
    AWB       string  // auto|... |custom
    AWBGainR, AWBGainB float64 // used when AWB=custom

    // Image tuning
    Brightness, Contrast, Saturation, Sharpness float64
    Denoise   string

    // Geometry
    HFlip, VFlip bool
    Rotation     int        // 0|180
    ROI          [4]float64 // normalised x,y,w,h; {0,0,1,1} = full (digital zoom)
}

type StillParams struct {
    Width, Height int    // default 4056x3040
    Encoding      string // jpg|png
    Quality       int    // JPEG 1..100
    Raw           bool   // also save DNG
    SettleMs      int    // AE/AWB settle time when auto-exposure
}
```

- Every field has a default, a validation range, and a **pure function
  `Config.VideoArgs() []string`** (and `StillArgs`) that is easy to unit-test.
- The current config is saved as JSON (`<dataDir>/config.json`) and loaded at
  startup.
- `PUT /api/config` accepts a partial JSON object that is merged onto the
  current config, so the UI can send only what changed.

---

## 5. Streaming layer

### 5.1 H.264 splitter (`internal/h264`)
`rpicam-vid` writes a raw Annex-B byte stream (`00 00 00 01 | NAL | 00 00 01 | NAL …`).
The splitter:
1. reads stdout in chunks (64 KiB) and scans for start codes;
2. classifies NALs by `nal[0] & 0x1F`: 7 = SPS, 8 = PPS, 5 = IDR slice,
   1 = non-IDR slice, others pass through;
3. puts together an **access unit (AU)**: any pending SPS/PPS/SEI followed by
   the VCL NAL(s) of one frame;
4. emits `AccessUnit{Data []byte (Annex-B), Key bool, TS time.Time}`.

**Latency note:** a NAL is known to be complete only when the next start code
arrives. That adds **up to one frame interval** (33 ms at 30 fps, 20 ms at
50 fps). The Pi encoder writes one slice per frame and the next frame follows
one frame interval later. This is the only reliable split point that the
`rpicam-vid` CLI gives us. Options to remove it later: (a) an "idle flush"
heuristic (flush when the pipe has been quiet for N ms); (b) the libcamera
helper mentioned in §4.1, which knows frame boundaries exactly. For v1 we
accept the +1 frame.

The splitter is a pure `io.Reader → chan AccessUnit` component. It is
unit-tested with recorded `.h264` files, including start codes split across
read boundaries.

### 5.2 Wire format of `GET /stream`
Response: `200`, `Content-Type: application/octet-stream`,
`Cache-Control: no-store`, `X-Accel-Buffering: no` (stops nginx buffering).
The body is a sequence of messages:

```
offset  size  field
0       1     type     1 = video AU, 2 = event (JSON), 3 = keepalive
1       1     flags    bit0 = keyframe, bit1 = contains SPS/PPS
2       2     reserved (0)
4       4     length   payload length, uint32 big-endian
8       8     ts       uint64 BE, µs since Unix epoch (server wall clock when the AU was completed)
16      N     payload  type 1: Annex-B H.264 AU; type 2: UTF-8 JSON; type 3: empty
```

Event payloads, for example:
```json
{"event":"state","state":"reconfiguring"}
{"event":"state","state":"streaming","width":1920,"height":1080,"fps":30,"codec":"h264"}
{"event":"state","state":"capturing"}
{"event":"error","message":"rpicam-vid exited: ..."}
```
A keepalive is sent every 2 s when no other data goes out (for example while a
still is being taken), so proxies do not close an idle connection.

### 5.3 Hub (`internal/stream`)
- Holds the set of subscribers. `Publish(au)` hands each AU to every
  subscriber **without blocking** the reader.
- Each subscriber has a bounded queue (default 30 messages, about 1 s) and its
  own writer goroutine that writes to its `http.ResponseWriter` and calls
  `Flush()`.
- **A new subscriber** gets the current state event first. Then it gets nothing
  until the **next keyframe**, which waits at most `Intra` frames (≤1 s by
  default).
- When `rpicam-vid` restarts, the Hub sends a `state` event and every
  subscriber goes back to "waiting for keyframe".

### 5.4 Backpressure and latency control (important for internet use)
TCP never drops data, so on a slow link latency would keep growing. The policy:
1. **Server-side drop-to-keyframe:** if a subscriber's queue is full (the
   client or link is slower than the stream), the Hub **drops everything for
   that subscriber until the next keyframe** and sends a counter in the next
   event. Latency stays bounded at about the queue length plus one GOP instead
   of growing without limit.
2. **Small kernel send buffer:** via `http.Server.ConnContext` we keep the
   `net.Conn` and call `SetWriteBuffer(256 KiB)` on stream connections. The
   backlog then shows up in our queue (where we can drop it) and does not hide
   in a multi-MB kernel buffer.
3. **Write deadline** per message (for example 5 s). A stuck client is
   disconnected and its resources are freed.
4. **Client-side:** if `decoder.decodeQueueSize` grows past 3, or the measured
   latency is above a threshold (default 400 ms) for several frames, the
   client skips decoding non-key frames until the next keyframe. We draw on
   `requestAnimationFrame` and show only the newest decoded frame (older ones
   are closed and never drawn).
5. **User controls:** resolution, fps, bitrate and GOP in the UI. Suggested
   presets: *LAN HQ* 1080p30 @ 12 Mbps, *Internet* 1080p30 @ 5 Mbps,
   *Mobile* 720p30 @ 2 Mbps, *Low-latency* 720p50 @ 4 Mbps with GOP 25.

### 5.5 Browser player (`static/player.js`, ~250 LOC, no libraries)
```
const res    = await fetch('/stream', {cache:'no-store', signal});
const reader = res.body.getReader();
// accumulate bytes → parse 16-byte header → payload
// type 1: if (flags & SPS) and the SPS changed → decoder.configure({
//            codec: avcCodecString(sps),     // "avc1.PPCCLL" from SPS bytes 1..3
//            optimizeForLatency: true,
//            hardwareAcceleration: 'prefer-hardware' })
//         // no `description` → the decoder expects Annex-B, so bytes go in as-is
//         decoder.decode(new EncodedVideoChunk({type: key?'key':'delta', timestamp: ts, data: payload}))
// type 2: update the overlay or status
// decoder output(frame): keep only the newest frame; on rAF → ctx.drawImage(frame), frame.close()
```
- **Reconnect** with backoff if the fetch ends or fails. The fetch is aborted
  while the page is hidden (`visibilitychange`) so it uses no bandwidth.
- **Stats overlay:** fps, bitrate (kbps), resolution, frames dropped, and an
  **estimated latency** = `clientNow − ts − clockOffset`. The offset comes
  from a simple NTP-style ping to `/api/time` (one round trip, take the minimum
  of 5). This latency covers the AU being ready on the Pi through display, which
  is useful for tuning. True glass-to-glass is measured by pointing the camera
  at a running stopwatch (see §10).
- If `window.VideoDecoder` is missing (no secure context or an old browser), a
  clear message explains why and links to MJPEG mode if it is enabled.

### 5.6 MJPEG fallback mode (optional, proposed for milestone 4)
- `Config.Codec = "mjpeg"` runs `rpicam-vid --codec mjpeg -q <quality>`, using
  the hardware JPEG encoder.
- The splitter for MJPEG cuts on JPEG SOI/EOI markers (`FFD8` … `FFD9`). EOI
  marks the frame end exactly, so there is no +1 frame latency.
- `GET /stream.mjpeg` serves `multipart/x-mixed-replace` and the page uses a
  plain `<img src="/stream.mjpeg">`. It works over plain HTTP and in any
  browser.
- It is a **global** mode, because only one encoder can own the camera. Meant
  for the LAN only (high bandwidth).

---

## 6. Still capture flow

```
Browser                         Server                       Camera
  │ POST /api/capture {params}    │                             │
  │──────────────────────────────▶│ Manager.CaptureStill        │
  │                               │ Hub event: capturing ──────▶ viewers show overlay
  │                               │ stop rpicam-vid (SIGINT) ──▶│
  │                               │ run rpicam-still ──────────▶│ (settle ~1 s if auto-exp,
  │                               │◀──── file written ──────────│  or --immediate if manual)
  │                               │ start rpicam-vid ──────────▶│
  │◀─ 200 {name,url,size,w,h} ────│ Hub event: streaming        │
  │ show thumbnail and download link                            │
```
- Expected stream gap: about 1.5–3 s with auto exposure, about 1–1.5 s with
  manual exposure.
- The request blocks until the still is done (timeout 30 s; long manual
  exposures add their shutter time). A second capture while one is running gets
  `409 Conflict`.
- Files go to `<dataDir>/captures/IMG_YYYYMMDD_HHMMSS[.jpg|.png|.dng]`.
- A **gallery** lists the files (`GET /api/captures`). Thumbnails are the
  images themselves scaled by the browser (`loading="lazy"`), so the Pi does no
  server-side thumbnailing. There are also download and delete actions.

---

## 7. Web layer (`internal/web`)

### 7.1 HTTP API

| Method | Path | Body / Response |
|---|---|---|
| GET | `/` | HTML page (`html/template`), current config and capabilities rendered into the form |
| GET | `/static/{file}` | embedded JS/CSS (`embed.FS`) |
| GET | `/stream` | framed H.264 stream (§5.2) |
| GET | `/stream.mjpeg` | MJPEG multipart (fallback mode only) |
| GET | `/api/config` | `Config` JSON |
| PUT | `/api/config` | partial `Config` JSON → `200` with the applied `Config`, or `400 {"error":"..."}` with validation errors. Restarts the stream |
| POST | `/api/config/reset` | restore defaults |
| GET | `/api/capabilities` | sensor modes, ranges, allowed enum values, H.264 limits |
| GET | `/api/status` | state, uptime, viewers, fps, bitrate, restarts, last error, CPU temperature (`/sys/class/thermal/thermal_zone0/temp`) |
| GET | `/api/time` | `{"now_us": ...}` for the clock-offset estimate |
| POST | `/api/capture` | `StillParams` JSON → `{name, url, size, width, height, raw_url?}` |
| GET | `/api/captures` | list `[{name, size, time, url}]` |
| GET | `/captures/{name}` | file download (`http.ServeFile`, name checked against the list to block path traversal) |
| DELETE | `/captures/{name}` | delete |

The router is a stdlib `http.ServeMux` with Go 1.22 patterns
(`"PUT /api/config"`, `"GET /captures/{name}"`).

### 7.2 Security (it may be exposed to the internet)
- **HTTP Basic auth** middleware is enabled when `-auth user:password` (or the
  env var `CAMERA_AUTH`) is set. Same-origin `fetch` sends the credentials
  automatically. Use it **only together with TLS**.
- **TLS:** `-tls-cert/-tls-key`, or `-tls-self-signed` (generate and keep in
  `<dataDir>/tls/`), or plain HTTP behind a TLS-terminating proxy or tunnel.
- `PUT`/`POST`/`DELETE` check the `Origin` header against the host (simple
  CSRF protection).
- `Config` validation keeps all values within safe ranges. Args go to
  `exec.Command` as a list (no shell), so command injection is impossible.

### 7.3 UI layout (single page)

```
┌───────────────────────────────────────────────┬──────────────────────────┐
│                                               │ ▸ Presets [Internet ▾]   │
│                                               │ ▸ Stream                 │
│           <canvas> live video                 │   Resolution [1920x1080▾]│
│                                               │   FPS [30] Bitrate [6M]  │
│   overlay: "reconfiguring…" / "capturing…"    │   GOP [30] Sensor [auto▾]│
│                                               │ ▸ Exposure               │
│ ┌────────────────────────────┐                │   (•)Auto ( )Manual      │
│ │ 30fps 5.8Mbps 1920x1080    │                │   Shutter [====|  ] 1/250│
│ │ latency ~110ms dropped 0   │                │   Gain [==|    ] 2.0     │
│ └────────────────────────────┘                │   EV, Metering, Mode     │
├───────────────────────────────────────────────┤ ▸ White balance          │
│ [⛶ Fullscreen] [🔍 1:1 zoom]  [📷 Capture]    │ ▸ Image (bri/con/sat/shp)│
├───────────────────────────────────────────────┤ ▸ Orientation & zoom     │
│ Gallery: [thumb][thumb][thumb] …              │ ▸ Still (res, fmt, raw)  │
│                                               │ [Apply]  [x] auto-apply  │
└───────────────────────────────────────────────┴──────────────────────────┘
```
- Sections are `<details>` elements, so collapsing needs no JS.
- Because every change restarts the pipeline, there is an **Apply** button, and
  an optional **auto-apply** that waits 700 ms after the last change. That
  stops a slider drag from causing a restart storm.
- The shutter slider is logarithmic (100 µs to 1 s for video; longer exposures
  only for stills, up to the IMX477 maximum of about 670 s).
- The "1:1 zoom" button sets the ROI to a centred crop. With the HQ camera's
  manual lens this helps with **focusing**.
- Responsive CSS: on narrow (mobile) screens the panel goes below the video.

---

## 8. Project layout

```
camera_rpi/
├── go.mod                      module github.com/mhbvr/camera_rpi (no requires)
├── cmd/camweb/main.go          flags, wiring, signal handling, graceful shutdown
├── internal/
│   ├── camera/
│   │   ├── config.go           Config, defaults, validation, merge, VideoArgs/StillArgs
│   │   ├── config_test.go
│   │   ├── capabilities.go     parse `rpicam-hello --list-cameras`
│   │   ├── process.go          start/stop subprocess, stderr logging
│   │   ├── manager.go          state machine (single goroutine)
│   │   └── fake.go             fake source for development without a Pi
│   ├── h264/
│   │   ├── annexb.go           start-code scanner, AU assembler, SPS → codec string
│   │   └── annexb_test.go      (+ testdata/*.h264)
│   ├── mjpeg/                  (milestone 4) SOI/EOI splitter
│   ├── stream/
│   │   ├── hub.go              subscribers, queues, drop-to-keyframe
│   │   ├── wire.go             message framing (§5.2)
│   │   └── hub_test.go
│   └── web/
│       ├── server.go           routes, middleware (auth, origin check, logging)
│       ├── handlers.go
│       ├── tls.go              self-signed cert generation
│       ├── templates/index.html
│       └── static/{app.js, player.js, style.css}
├── docs/DESIGN.md
├── deploy/camweb.service       systemd unit
└── README.md
```

### Command-line flags (all optional)
```
-listen :8080              address
-data ./data               config.json, captures/, tls/
-auth user:pass            enable basic auth (or env CAMERA_AUTH)
-tls-cert, -tls-key        use given certificate
-tls-self-signed           generate a self-signed cert on first run
-rpicam-prefix rpicam      use "libcamera" on Bullseye
-fake path/to/file.h264    no camera: loop a recorded H.264 file at the configured fps
-log-level info
```

### Build and deploy
```
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o camweb ./cmd/camweb   # 64-bit Pi OS
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o camweb ./cmd/camweb   # 32-bit
scp camweb pi@raspberrypi:~ && ssh pi@raspberrypi ./camweb -tls-self-signed
```
A systemd unit (`deploy/camweb.service`) runs it at boot as the `pi` user (who
is in the `video` group).

---

## 9. Latency budget (expected, 1080p30)

| Stage | Estimate |
|---|---|
| Sensor exposure + readout | ~33 ms (one frame time; less with fps 50) |
| ISP + H.264 encode (HW) | 10–20 ms |
| AU split (+1 frame, §5.1) | ≤33 ms |
| Server queue → socket | ~1 ms (when not congested) |
| Network | LAN 1–5 ms; internet RTT/2 + I-frame serialisation (150 KB at 10 Mbps ≈ 120 ms burst) |
| Browser HW decode | 5–15 ms |
| Display (next vsync) | 0–16 ms |
| **Total** | **LAN ≈ 80–130 ms**, internet ≈ 150–300 ms depending on link |

Levers: a higher fps (50) shortens every frame-based stage, a smaller GOP
speeds recovery, a lower bitrate makes I-frame bursts smaller, and removing the
+1 frame delay comes later (§5.1).

---

## 10. Testing plan
- **Unit tests** (run anywhere, no Pi): Annex-B splitter (golden files, start
  codes split across reads, SPS change), Config validation/merge/args mapping,
  Hub drop-to-keyframe policy, wire framing, capabilities parsing (sample
  `--list-cameras` output).
- **Fake camera** (`-fake file.h264`): the whole web app runs on a laptop or in
  CI with a recorded or generated H.264 stream
  (`ffmpeg -f lavfi -i testsrc=size=1920x1080:rate=30 -c:v libx264 -bf 0 -g 30 test.h264`).
  The Playwright browser can then do an end-to-end check that frames are
  decoded.
- **On the Pi 4 + HQ camera** (manual):
  1. glass-to-glass latency: point the camera at a ms stopwatch on the monitor
     and photograph both;
  2. 1080p30 at 5 Mbps over the internet (phone on LTE): stable, no latency
     creep;
  3. throttle the client network (Chrome DevTools) and check that latency stays
     bounded (drop-to-keyframe works);
  4. change every parameter in the UI and check it takes effect;
  5. still at 4056×3040 JPEG + DNG, and the stream recovers;
  6. kill `rpicam-vid` by hand and check the supervisor restarts it;
  7. CPU and temperature during streaming with 3 viewers.

---

## 11. Milestones (after design approval)
1. **M1 — Live stream:** project skeleton, `rpicam-vid` process, H.264 splitter,
   Hub, `/stream`, minimal page with the WebCodecs player, stats overlay, fake
   source. *Goal: measure real latency on the Pi early.*
2. **M2 — Configuration:** Config model, validation, args, persistence,
   capabilities, `/api/config`, settings UI, presets, auto-apply.
3. **M3 — Stills:** `rpicam-still` flow in the Manager, `/api/capture`,
   gallery, download, delete.
4. **M4 — Hardening:** TLS (self-signed and provided), Basic auth, origin
   check, backpressure tuning, status page, systemd unit, README, optional
   MJPEG fallback.

---

## 12. Open questions for review
1. **HTTPS requirement:** WebCodecs needs a secure context. Is this acceptable:
   a self-signed cert on the LAN and a reverse proxy or tunnel for the internet?
   Or should **MJPEG over plain HTTP** be a first-class mode on the LAN (moved
   into M1/M2)?
2. **Internet transport:** are you OK with TCP (streamed HTTP) for v1, with
   WebRTC kept as an upgrade path? Or is internet use on lossy or mobile links
   central enough to need WebRTC now (which brings in the `pion` dependency)?
3. **Stream interruption:** is a short 0.5–1 s blip on every config change, and
   a 1.5–3 s blip on every still, acceptable for the prototype?
4. **Pi OS version / architecture:** Bookworm 64-bit? (This affects the binary
   name prefix and the build target.)
5. **Auth:** is Basic auth over TLS enough, or is something else needed (for
   example a share link with a token)?
6. **Stills:** is DNG raw output wanted? Should we keep EXIF, timestamps and a
   capture history?
7. **Multiple viewers:** is it fine that all viewers share one quality setting?
