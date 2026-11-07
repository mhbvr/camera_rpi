# Architecture

camweb is a prototype web application for a **Raspberry Pi 4 with the HQ
Camera (IMX477)**:
1. a **low-latency live stream** in the browser (1080p-class H.264 at a few
   Mbit/s, usable on the LAN and over the internet);
2. **camera settings** applied live where the hardware allows it;
3. **snapshots without stopping the stream** (2028×1520 JPEG from the camera's
   second ISP output).

Goals: few dependencies (Go standard library, system Python with Picamera2,
no JS libraries), simple code, and a camera part that can be replaced (the
Python helper may become C++ later).

Related documents: [REST_API.md](REST_API.md) (HTTP API),
[HELPER_API.md](HELPER_API.md) (Go ↔ helper protocol),
[HARDWARE.md](HARDWARE.md) (camera pipeline and settings),
[HW_SPECIFIC.md](HW_SPECIFIC.md) (portability), [SPIKE.md](SPIKE.md)
(measurements).

Contents:
1. [Components](#1-components)
2. [Division of responsibility](#2-division-of-responsibility)
3. [Processes and IPC](#3-processes-and-ipc)
4. [Go server](#4-go-server)
5. [Streaming to the browser](#5-streaming-to-the-browser)
6. [Security](#6-security)
7. [Latency](#7-latency)
8. [Risks and limitations](#8-risks-and-limitations)
9. [Repository layout](#9-repository-layout)
10. [Testing and development without a Pi](#10-testing-and-development-without-a-pi)
11. [Out of scope and future work](#11-out-of-scope-and-future-work)

---

## 1. Components

```
┌───────────────────────────────┐  ctl.sock: JSON-RPC 2.0   ┌───────────────────────────────┐
│ camhelper.py                  │◀─────────────────────────▶│ camweb                        │  HTTP(S)
│ Python, Picamera2, libcamera  │                           │ Go, standard library only     │◀─────────▶ browser page,
│ camera, ISP, HW encoders,     │  video.sock: framed video │ config, fan-out, REST API,    │            other clients
│ snapshots                     │──────────────────────────▶│ page; spawns the helper       │
└───────────────────────────────┘                           └───────────────────────────────┘
```

- **camhelper.py** (`camhelper/`, embedded into the camweb binary): opens the
  camera with Picamera2, runs the pipeline and the hardware encoders, captures
  snapshots, reports metadata.
- **camweb** (`cmd/camweb`): one static Go binary. Spawns and supervises the
  helper, owns the configuration, fans the video out to clients, serves the
  page and the REST API.
- **Web page** (`internal/web/`): `html/template` + vanilla JS. Uses only the
  public REST API and streams.

### 1.1 Pipelines

One sensor mode feeds the ISP, which produces two images per frame: `main`
for snapshots and `lores` for video. Three pipelines use them:

```
 camhelper.py (Python, Picamera2)                                 camweb (Go)                                clients
 ────────────────────────────────                                 ───────────                                ───────

                ┌─ main  YUV 2028×1520 ─▶ snapshot (copy + JPEG) ─▶ file in captures/ ─────────────────────▶ POST /api/capture   download
                │                          on request, stream keeps running
 IMX477 ─▶ ISP ─┤
                │                       ┌─▶ H264Encoder (HW) ══╗               ┌─▶ stream.Hub ───────────┬─▶ /stream        page (WebCodecs)
                │                       │   always             ║               │   drop-to-keyframe      └─▶ /stream.h264   players
                └─ lores YUV 1440×1080 ─┤                      ╠═ video.sock ═▶┤
                                        │                      ║ H.264 + JPEG  │
                                        └─▶ MJPEGEncoder (HW) ═╝               └─▶ stream.FrameHub ────────▶ /stream.mjpeg  fallback <img>
                                            every n-th frame,                      latest frame wins
                                            only while needed ◀── set_mjpeg ◀── camera.Manager ◀── MJPEG client count
```

- **H.264 (main stream, always on while streaming).** The hardware H.264
  encoder runs on `lores`. Each encoded frame goes over `video.sock` as type 1
  to `stream.Hub`, which fans it out to `/stream` (the page, decoded with
  WebCodecs) and `/stream.h264` (players), dropping to the next keyframe for
  clients that fall behind (§4.2).
- **MJPEG (fallback, on demand).** A second hardware encoder, `MJPEGEncoder`,
  runs on the **same `lores` stream** and encodes every n-th frame
  (`mjpeg.skip`). It runs only while at least one client is connected to
  `/stream.mjpeg`: `stream.FrameHub` reports its client count to
  `camera.Manager`, which sends `set_mjpeg {enabled: true|false}` to the
  helper. Each JPEG goes over `video.sock` as type 4 to `FrameHub`, which keeps
  only the newest frame per client (§4.3). The page uses it when the browser
  cannot decode H.264 (§5.3). It does not affect the H.264 stream (spike 0.10).
- **Snapshot (on request).** `POST /api/capture` → `snapshot` over `ctl.sock`
  → the helper copies the next `main` frame (2028×1520), encodes the JPEG in a
  thread, writes it to `<data>/captures/`, and camweb returns the file. Video
  keeps running.
- **Control and events** (not drawn): settings go from `camera.Manager` to
  the helper over `ctl.sock` (`configure`, `set_controls`, `set_encoder`,
  `set_mjpeg`); the helper's `state`, `metadata` and `stats` notifications go
  back and are published through `stream.Hub` to `/stream`, `/api/events` and
  `/api/status`.

## 2. Division of responsibility

- **The helper is thin.** It does only what needs the camera: configure the
  pipeline, apply libcamera controls, encode, capture and save stills, report
  metadata. It holds **no policy** and stores no user configuration.
- **Go owns everything else**: the configuration model, validation, defaults,
  persistence, the mapping to libcamera control names
  (`internal/camera/controls.go`), deciding how disruptive a change is, file
  naming, HTTP, fan-out and backpressure.
- They talk only through [HELPER_API.md](HELPER_API.md). Another helper that
  implements it (C++, another camera stack) replaces the Python one without
  changes to the server.

## 3. Processes and IPC

### 3.1 What crosses the boundary

| Flow | Volume |
|---|---|
| H.264 video, helper → Go | ≤ 25 Mbit/s ≈ 3 MB/s worst case, 30–40 messages/s, 5–500 KB each |
| MJPEG video (only while MJPEG clients watch) | up to 25 Mbit/s, 35–62 KB per frame at 1440×1080 |
| Control requests and replies | a few per user action, < 1 KB |
| Events (state, metadata, stats) | ~2–5 per second, < 1 KB |
| Snapshots | **not sent over IPC**: the helper writes the file, Go reads it |

### 3.2 Why two Unix domain sockets

| | stdin/stdout pipes | **Unix domain sockets** | Shared memory |
|---|---|---|---|
| Latency per message | ~10–30 µs | ~10–30 µs (0.34 ms for 200 KB, spike 0.8) | ~1–5 µs plus synchronisation |
| Cost at 3 MB/s | negligible | negligible | negligible; zero-copy gains nothing here |
| Separate control and video channels | awkward | **natural** (two sockets) | needs a side channel |
| Process lifecycle | tied to the parent | **independent**: either side can restart and reconnect | independent, crash cleanup is tricky |
| Stray `print()` or library output | **corrupts the stream** | harmless | harmless |
| Debugging | hard | `socat - UNIX-CONNECT:ctl.sock` | needs tools |
| Later zero-copy | no | yes: pass memfd/dmabuf fds (`SCM_RIGHTS`) | — |
| Complexity | lowest | low | highest |

Latency and throughput do not decide at this load; robustness and simplicity
do. So:
- `ctl.sock`: **JSON-RPC 2.0**, one message per line (requests, responses,
  notifications). Human-readable and scriptable. gRPC or HTTP over the socket
  would cost more for no gain.
- `video.sock`: a **32-byte length-prefixed header** per frame. A raw Annex-B
  byte stream would force Go to wait for the next start code to find frame
  boundaries (+1 frame of latency) and loses timestamps; RTP or MPEG-TS need
  muxing and demuxing; `SOCK_SEQPACKET` hits size limits with large I-frames.

Shared memory is deferred until raw frames must cross the boundary (for
example image processing in Go). It can be added without redesign: the helper
passes a memfd ring buffer over `ctl.sock` with `SCM_RIGHTS`
([HELPER_API.md §7](HELPER_API.md#7-future-extensions-reserved-not-in-v1)).

### 3.3 Topology and supervision
- **Go listens, the helper connects.** camweb creates both sockets (mode 0600)
  in the runtime directory (`$XDG_RUNTIME_DIR/camweb/` or `<data>/run/`, or
  `-runtime-dir`), then starts the helper with the socket paths. No startup
  race; a restarted helper simply reconnects.
- `-helper spawn` (default): camweb writes the embedded `camhelper.py` to the
  runtime directory, runs `python3 camhelper.py --ctl … --video …` (or
  `-helper-cmd`), forwards its stderr to the log with a `[helper]` prefix, and
  restarts it with backoff (1 s, 2 s, 4 s … 30 s) when it exits or a socket
  closes.
- `-helper external`: camweb only listens, for a helper run by systemd, by
  hand, or under development.
- No heartbeat: a session is alive while the process runs and both sockets
  are open. If either socket drops, Go ends the session and (spawn mode)
  restarts the helper; viewers see the state `helper-restarting` and the stream
  resumes at the next keyframe (about 1 s after `kill -9`).
- On connect the helper sends `hello` (camera model, sensor modes, control
  ranges); Go then sends `configure` with the saved configuration.

## 4. Go server

Packages (`internal/`):

| Package | Role |
|---|---|
| `proto` | helper protocol types: video framing, JSON-RPC 2.0 messages |
| `helper` | socket listener, session (RPC calls matched by id, video reader goroutine), supervisor (spawn, restart with backoff) |
| `camera` | `Config` model, settings schema (ranges, validation), `Diff`, libcamera control mapping, `Manager` (drives the session), persistence |
| `stream` | `Hub` (H.264 + events fan-out, drop-to-keyframe), `FrameHub` (MJPEG, latest frame wins), browser wire format |
| `h264` | NAL scanning, SPS → codec string |
| `web` | HTTP server, REST handlers, template, static JS/CSS |

### 4.1 Configuration
- One JSON document split by how disruptive a change is: `pipeline`
  (reconfigure), `encoder` (encoder restart), `controls` (live; a frame-rate
  change also restarts the encoder), `still` (per capture), `mjpeg` (fallback
  encoder). Every field: [HARDWARE.md §4](HARDWARE.md#4-settings-reference).
- `POST /api/config` merges a partial document, validates it (the
  settings schema plus the camera's own gain and shutter maxima from `hello`), diffs
  it against the current one and sends the least disruptive helper calls:
  `set_controls`, then `set_encoder` or `configure`.
- Saved to `<data>/config.json`; applied after every `hello`, so it survives
  helper and server restarts.
- The schema (`internal/camera/schema.go`) is the only definition of ranges
  and allowed values: validation checks against it, `GET /api/schema`
  publishes it, and the page template reads its form ranges from it.

### 4.2 Fan-out and backpressure (`stream.Hub`)
- One encoded stream, N clients, each with a bounded queue (30 messages ≈ 1 s)
  filled by a non-blocking publish.
- A new client, or a new encoder session (`stream_start`), waits for the next
  keyframe.
- **Drop-to-keyframe**: when a client's queue is full, it loses everything up
  to the next keyframe and gets a `drop` event. Latency stays bounded on slow
  links. This is the only latency control; the browser does not skip frames.
- The kernel send buffer is kept small (`SetWriteBuffer(256 KiB)` via
  `http.Server.ConnContext`) so a backlog lands in the hub queue, where it can
  be dropped. Per-write deadline 5 s; keepalive after 2 s of silence.
- Sticky events: the latest `state`, `camera`, `stream`, `metadata` and
  `stats` are replayed to each new client and served by `GET /api/status`.
- `/stream`, `/stream.h264` and `/api/events` are all hub clients (events-only
  clients skip video).

### 4.3 MJPEG (`stream.FrameHub`)
"Latest frame wins": each client has a one-frame slot, so a slow client skips
frames and never falls behind. The client count drives the helper's MJPEG
encoder (`set_mjpeg`): on with the first client, off after the last, re-enabled
when a new helper session starts.

## 5. Streaming to the browser

### 5.1 Protocol choice
**H.264 in a streamed HTTP response (`fetch` + ReadableStream), decoded with
WebCodecs** ([wire format](REST_API.md#stream)).
- No dependencies on either side, low latency (no player buffer), plain
  HTTP(S) on one port (works through reverse proxies and tunnels).
- Rejected for the prototype: MJPEG as the main stream (several times the
  bandwidth; kept as fallback), HLS/LL-HLS (seconds of latency), MSE/fMP4
  (more buffering, needs a muxer), WebSocket (no server in Go's standard
  library), WebRTC (best on lossy links, but large dependencies such as `pion`
  plus ICE/STUN/TURN; it is the upgrade path), a media server such as mediamtx
  (an extra binary, no snapshot from the same pipeline).

### 5.2 Player (`internal/web/static/player.js`)
- `fetch('/stream')` → `getReader()` → parse the 16-byte headers.
- On a new SPS: `VideoDecoder.configure({codec: 'avc1.PPCCLL' from the SPS,
  optimizeForLatency: true})`, no `description` (chunks are Annex-B).
  `hardwareAcceleration` stays at its default: Chrome treats
  `prefer-hardware` as hardware-only and fails on machines without a hardware
  H.264 decoder.
- Only the newest decoded frame is drawn, on `requestAnimationFrame`.
- On end or error it reconnects every 2 s. It keeps running in hidden tabs.
- **WebCodecs needs a secure context** (HTTPS or `localhost`): built-in TLS
  (`-tls-cert`/`-tls-key` with a private CA from `make cert`, `cmd/certgen`),
  an SSH port forward, or an HTTPS tunnel/proxy. See the README.

### 5.3 MJPEG fallback
For browsers that cannot decode H.264 with WebCodecs: Firefox for Android has
no working WebCodecs H.264 yet (MDN lists `VideoDecoder` as unsupported there,
[VideoDecoder.json](https://github.com/mdn/browser-compat-data/blob/main/api/VideoDecoder.json);
Mozilla's [bug 1746557](https://bugzilla.mozilla.org/show_bug.cgi?id=1746557)
has no milestone). On HTTPS it does expose `VideoDecoder`, but decoding fails
with *"operation not supported"* (tested on a phone, 2026-11), so checking for
`VideoDecoder` is not enough.

- **Helper:** a second hardware encoder (`MJPEGEncoder`) on the same lores
  stream, every n-th frame, only after `set_mjpeg {enabled: true}`; each frame
  is one JPEG (video type 4).
- **Server:** `FrameHub` + `GET /stream.mjpeg` (`multipart/x-mixed-replace`).
- **Browser:**
  - no `VideoDecoder` (or `?mjpeg=1`): the page shows
    `<img src="/stream.mjpeg">` from the start;
  - otherwise it starts with H.264 and switches in place when, before any
    frame was decoded, `VideoDecoder.isConfigSupported()` says no,
    `configure()` throws, the decoder reports `NotSupportedError`, or it fails
    3 times. Later failures only reset the decoder;
  - a note under the video gives the reason;
  - events still come from `GET /stream?video=0`, so readouts keep working.
- **Trade-offs:** several times the bandwidth of H.264; low latency; works over
  plain HTTP; small Pi load (spike 0.10: no H.264 drops, +5 % CPU).

## 6. Security

- **No authentication.** Run camweb on a trusted network, or reach it through
  an SSH forward, Tailscale, or a tunnel/proxy with its own access control.
- Optional TLS (`-tls-cert`, `-tls-key`, HTTP/2 included); `make cert`
  creates a private CA (ECDSA P-256, may sign only server certificates) and a
  server certificate.
- CORS is off unless `-cors-origin` is given.
- Validation is strict: the helper receives only validated, typed values; file
  names are generated by Go, never taken from a client; request bodies are size
  limited.
- The IPC sockets are mode 0600 in a private runtime directory.

## 7. Latency

Budget for 1440×1080 at 30 fps on a LAN (camera-side stages measured, spike
0.2–0.8):

| Stage | Time |
|---|---|
| exposure, readout, ISP (sensor timestamp → request complete) | 35 ms p50 (33 ms exposure, dark scene; less with shorter exposures) |
| hardware H.264 encode | ~17 ms p50 (sensor → encoder output 52 ms p50, 62 ms p95) |
| helper hand-off (Python, socket) | < 1 ms; snapshot JPEG encoding adds up to ~9 ms jitter (p95) |
| Go hub and HTTP write | ~1 ms |
| network | LAN 1–5 ms; internet RTT/2 plus I-frame serialisation |
| browser hardware decode | 5–15 ms |
| display (next vsync) | 0–16 ms |
| **total** | **LAN ≈ 60–100 ms**; internet ≈ 120–300 ms |

The `stats` event reports the helper's measured sensor → socket latency;
glass-to-glass is measured by filming an on-screen millisecond clock.

## 8. Risks and limitations

| Risk | Status / mitigation |
|---|---|
| Python GIL: a snapshot's JPEG encode delays the encoder output thread | measured (spike 0.5): callback interval p95 34.5 → 43 ms, no dropped frames. Accepted; a subprocess encoder is the fix if needed |
| No helper-side queue: if Go stops reading `video.sock`, the encoder blocks | Go reads it in a dedicated goroutine that only hands frames to the non-blocking hub; 1 MiB `SO_SNDBUF` absorbs hiccups |
| Picamera2 API differences between versions | the helper probes optional arguments (`profile`, `framerate`) and accepts extra `outputframe` args |
| TCP delay build-up over the internet | drop-to-keyframe, small send buffer; WebRTC is the upgrade path |
| WebCodecs needs a secure context | built-in TLS, SSH forward or HTTPS tunnel; the page explains it and falls back to MJPEG |
| Hardware assumptions (sensor mode, sizes, tuning tables, encoder quirks) | concentrated in `internal/camera/config.go` and the helper; see [HW_SPECIFIC.md](HW_SPECIFIC.md) |

## 9. Repository layout

```
cmd/camweb/        server entry point (flags, wiring)
cmd/fakecam/       fake helper in Go + testdata/test.h264 (development without a Pi)
cmd/certgen/       private CA + server certificate (make cert)
camhelper/         camhelper.py (embedded into camweb), test_camhelper.py, fake/ Picamera2 + libcamera
internal/proto/    helper protocol: video framing, JSON-RPC 2.0 types
internal/helper/   sockets, session, supervisor
internal/camera/   config model, limits, schema, libcamera mapping, Manager
internal/stream/   Hub, FrameHub, browser wire format
internal/h264/     NAL/SPS helpers
internal/web/      HTTP server, REST API, template, static JS/CSS
spike/             hardware spike script (docs/SPIKE.md)
deploy/            systemd unit
docs/              this documentation
```

## 10. Testing and development without a Pi

| Layer | How |
|---|---|
| Go packages | unit tests (`make test`: `go vet`, `go test -race`), including HTTP handler tests for every endpoint and a test that the schema covers every config field |
| helper protocol, Go side | tests with an in-process fake helper over real Unix sockets |
| Python helper | `camhelper/test_camhelper.py` (framing, dispatcher, JPEG colour round trip, encoder rate control and its fallback) on a fake Picamera2 (`camhelper/fake/`) |
| end to end without a Pi | `make run-fake` (camweb + `cmd/fakecam` replaying `testdata/test.h264`) or `make run-fake-py` (camweb + the real `camhelper.py` on the fake Picamera2), checked in headless Chromium |
| hardware | `spike/m0_spike.py` on the Pi ([SPIKE.md](SPIKE.md)) |

Dependencies: Go standard library only; system `python3` + `python3-picamera2`
(no pip); no browser libraries; `ffmpeg` only to regenerate test streams.

## 11. Out of scope and future work

Not in the prototype: authentication, capture gallery, full-resolution 12 MP
capture (mode switch), DNG raw, recording on the Pi, WebRTC, image
processing, other sensor modes (16:9, high fps), digital zoom, H.264 profile
selection, flips in the UI, presets.

Possible next steps:
- **C++ helper** on libcamera + V4L2 encoders + libjpeg-turbo, implementing
  [HELPER_API.md](HELPER_API.md) unchanged, validated by a protocol
  conformance test suite (to be written).
- `capture_full` (12 MP) and DNG
  ([HELPER_API.md §7](HELPER_API.md#7-future-extensions-reserved-not-in-v1)).
- Shared-memory frame sharing for image processing.
- WebRTC for lossy internet links.
- Support for other boards and cameras ([HW_SPECIFIC.md](HW_SPECIFIC.md)).
