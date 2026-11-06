# camera_rpi — Design Document (v0.2, for review)

A web application for a Raspberry Pi 4 with the HQ Camera (Sony IMX477):

1. a **low-latency live stream** in the browser, usable on the LAN and over the
   internet at HQ (1080p) with reasonable bandwidth;
2. **camera settings** (exposure, gain, white balance, image tuning, frame
   rate, resolution, zoom, bitrate…), applied **live** where the hardware
   allows it;
3. **stills taken without stopping the stream** (quick snapshot), plus
   **full-resolution 12 MP stills** (JPEG/PNG, optional DNG raw) with a short
   pause.

It is a prototype: keep it simple, keep dependencies few, and make the camera
part replaceable (the Python helper is expected to become C++ later).

Related documents:
- [`PROTOCOL.md`](PROTOCOL.md) — the IPC contract between the Go server and
  the camera helper (what a future C++ helper must implement)
- [`PLAN.md`](PLAN.md) — implementation plan, milestones, tasks, acceptance
  criteria

### Changes since v0.1
- `rpicam-vid` / `rpicam-still` subprocesses are replaced by a **long-running
  Python helper using Picamera2**. It talks to libcamera directly and uses its
  two ISP outputs: H.264 video from `lores`, stills from `main`.
- Most settings now apply **live** with no pipeline restart.
- Stills: a **snapshot without interrupting the stream** (main stream,
  ~3 MP), plus a **full-res capture** by switching mode inside the helper (no
  process restart).
- The H.264 splitter and its extra one-frame delay are gone. The encoder
  hands over whole frames.
- IPC between Go and the helper uses **Unix domain sockets** (§4).

---

## 1. Constraints and assumptions

| Item | Assumption |
|---|---|
| Hardware | Raspberry Pi 4, HQ Camera (IMX477) on CSI, manual-focus C/CS lens |
| OS | Raspberry Pi OS Bookworm or newer, 64-bit preferred |
| Camera stack | libcamera + **Picamera2** (`python3-picamera2`). Preinstalled on desktop images; on Lite run `sudo apt install -y python3-picamera2 --no-install-recommends` |
| Go | 1.22+, **stdlib only**, `CGO_ENABLED=0`, one static binary |
| Python | System `python3` with Picamera2 and numpy (pulled in by Picamera2). **No pip packages** |
| Frontend | `html/template` + vanilla JS, **no JS libraries**; WebCodecs for decoding |
| Browsers | Current Chrome / Edge / Firefox (130+) / Safari (16.4+) |

### Facts about the Pi 4 camera system (from the Picamera2 manual)
- The ISP produces **up to two images per sensor frame**: **main** (RGB or YUV)
  and **lores** (must be **YUV** on Pi 4, and **no larger than main**). The
  **raw** sensor frame can be delivered at the same time.
- main and lores share the transform (flips) and colour space. They are scaled
  from the same sensor crop, so **lores must have the same aspect ratio as
  main** or the picture is stretched (to be confirmed in the M0 spike).
- **One sensor mode drives every stream.** It sets the maximum frame rate and
  the largest image available. IMX477 modes:

  | Sensor mode | Max fps | Use |
  |---|---|---|
  | 1332×990 10-bit | 120 | high fps, cropped FOV |
  | 2028×1080 12-bit | 50 | 16:9, cropped FOV |
  | 2028×1520 12-bit (2×2 binned) | 40 | **default: 4:3 full FOV** |
  | 4056×3040 12-bit | 10 | full-res stills only |

- **Hardware H.264 encoder**: max 1920×1080 (level 4.2). The Pi 4 also has a
  hardware MJPEG encoder. Still JPEG encoding is **in software** (Picamera2
  0.3.26+ can encode YUV420 straight to JPEG).
- Runtime controls (`set_controls`) apply while running, a few frames later
  for exposure and gain. Changing the sensor mode, stream sizes or transform
  needs a reconfigure (stop, configure, start) **inside the process**.
- Camera buffers come from **CMA** (320 MB by default on Pi 4 models with more
  than 1 GB RAM). One 4056×3040 YUV420 buffer is about 18.5 MB, so full-res
  configurations must use few buffers.

---

## 2. High-level architecture

```
                                Raspberry Pi 4
┌────────────────────────────────────────────────────────────────────────────────┐
│                                                                                │
│  camhelper.py (Python, Picamera2)                 camweb (Go, stdlib only)     │
│  ┌──────────────────────────────────┐            ┌───────────────────────────┐ │
│  │ libcamera ── ISP ─┬─ main  (YUV) ─┼─ snapshot ─▶ files in captures/        │ │
│  │   IMX477          │                │  /full-res  ▲  (helper writes, Go serves)│
│  │                   └─ lores (YUV) ─┤             │                          │ │
│  │                        │          │             │                          │ │
│  │                  H264Encoder (HW) │             │                          │ │
│  │                        │ whole AU │  video.sock │  ┌──────────────┐        │ │
│  │                  sender thread ───┼─────────────┼─▶│ helper.Client │        │ │
│  │                                   │  (binary)   │  │  video reader ├─▶ Hub ─┼─┐
│  │  control loop  ◀──────────────────┼─────────────┼──│  ctl JSON-RPC │        │ │ │
│  │  (configure, set_controls,        │   ctl.sock  │  └──────▲───────┘        │ │ │
│  │   snapshot, capture_full)  ───────┼─────────────┼─▶ events (state, metadata) │ │
│  └──────────────────────────────────┘ (JSON lines) │  ┌──────┴───────┐        │ │ │
│            ▲ spawned + supervised by Go             │  │camera.Manager│ config │ │ │
│            └────────────────────────────────────────┼──│ policy, diff,│ persist│ │ │
│                                                     │  │ state        │        │ │ │
│                                                     │  └──────▲───────┘        │ │ │
│                                                     │  ┌──────┴───────────────┐│ │ │
│                                                     │  │ web.Server (net/http) ││◀┘ │
│                                                     │  └───────────▲───────────┘│   │
│                                                     └──────────────┼────────────┘   │
└────────────────────────────────────────────────────────────────────┼────────────────┘
                                                                     │ HTTPS
                       Browser: fetch('/stream') → parser → VideoDecoder (WebCodecs) → <canvas>
                                REST: /api/config, /api/controls, /api/capture, /api/captures
```

### Division of responsibility (central for the later C++ rewrite)
- **The helper is thin.** It owns the camera and does only what needs
  libcamera: configure pipelines, apply controls, encode H.264, capture and
  save stills, report metadata. It holds **no policy** and stores no user
  config.
- **Go owns everything else**: the user-facing config model, validation,
  defaults, persistence, deciding how disruptive a change is, file naming,
  HTTP, auth, fan-out to viewers, backpressure.
- They talk only through the protocol in [`PROTOCOL.md`](PROTOCOL.md). A C++
  helper that implements it is a drop-in replacement, and a Go **conformance
  test** checks any helper against the protocol.

---

## 3. Camera pipeline (inside the helper)

### 3.1 Stream profiles
Because of the rules in §1 (one sensor mode; lores ≤ main with the same aspect
ratio; H.264 ≤ 1080p), Go exposes **profiles** instead of free-form sizes:

| Profile | Sensor mode | main (snapshot source) | lores → H.264 (choices) | fps |
|---|---|---|---|---|
| `4:3` (default) | 2028×1520 | 2028×1520 (3.1 MP) | 1440×1080, 1024×768, 640×480 | 1–40 |
| `16:9` | 2028×1080 | 1920×1080 (2.1 MP) | 1920×1080, 1280×720, 854×480 | 1–50 |
| `fast` | 1332×990 | 1332×990 (1.3 MP) | 1280×960, 640×480 | 1–120 |

The exact sizes and the cropping behaviour are checked in the M0 spike and
then fixed in the profile table in Go (`internal/camera/profiles.go`). The
helper takes explicit sizes, so changing profiles never touches the helper.

Picamera2 video configuration (conceptually):
```python
cfg = picam2.create_video_configuration(
    main={"size": main_size, "format": "YUV420"},
    lores={"size": stream_size, "format": "YUV420"},
    sensor={"output_size": mode_size, "bit_depth": mode_bits},
    transform=Transform(hflip=h, vflip=v),
    buffer_count=buffer_count,               # default 6, tuned in M0
    controls={"FrameDurationLimits": (d, d)},
)
picam2.configure(cfg)
picam2.start_encoder(H264Encoder(bitrate=..., repeat=True, iperiod=gop, profile=...),
                     SocketOutput(...), name="lores")
picam2.start()
```
`repeat=True` puts SPS/PPS before every IDR, so any viewer can start at any
keyframe.

### 3.2 What each kind of change costs

| Change | Mechanism | Stream impact |
|---|---|---|
| Exposure (auto/manual, shutter, gain, EV, metering, mode), AWB (mode, manual gains), brightness, contrast, saturation, sharpness, noise reduction, **zoom/ROI** (`ScalerCrop`), **fps within the mode** (`FrameDurationLimits`) | `set_controls` (live) | **none**, takes effect a few frames later |
| Bitrate, GOP, H.264 profile | restart the encoder only | new IDR, a gap of tens of ms (to measure) |
| Profile / stream size / sensor mode / flips / buffer count | reconfigure inside the helper (stop → configure → start) | short gap, a few hundred ms expected (to measure) |
| Helper crash | Go restarts the helper | about 1–3 s (process and camera init) |

### 3.3 Stills
**Quick snapshot (no interruption).** The helper grabs the next completed
request, copies the `main` buffer (~4.6 MB for 2028×1520 YUV420), releases the
request at once, and encodes JPEG/PNG in a worker thread. The H.264 stream on
lores keeps running. If raw is requested, DNG is saved from the raw stream
when one is configured (sensor-mode resolution). The default video config
leaves out raw to save CMA; with "raw on snapshot" enabled, the pipeline is
reconfigured once to include it.

**Full-resolution capture (short pause).**
```
stop_encoder → switch_mode(still_cfg: main 4056×3040 YUV420 [+ raw], buffer_count=2)
             → capture_request → copy main [+ raw] → release
             → switch_mode(video_cfg) → start_encoder
             → (worker thread) encode JPEG/PNG, write DNG
```
- The encoder is stopped and restarted explicitly around the mode switch. This
  is the safe path; whether the encoder can stay attached is checked in M0.
- The buffers are copied and encoding happens **after** switching back, so
  the stream pause covers only the two mode switches and one frame (expected
  well under a second; measured in M0).
- AE/AWB state is carried across the switch (the manual says the
  `switch_mode` delay brings no benefit on Pi 4). If the M0 spike shows
  drift, the helper copies the last `ExposureTime`/`AnalogueGain`/`ColourGains`
  into the still config as fixed values.
- Files are written to a temp name and then renamed, so Go never serves a
  half-written file.

### 3.4 Metadata
A Picamera2 `post_callback` samples per-frame metadata at about 2 Hz:
`ExposureTime`, `AnalogueGain`, `DigitalGain`, `ColourGains`,
`ColourTemperature`, `Lux`, `FrameDuration`, `ScalerCrop`, and `FocusFoM`
if present. These are sent as events. The UI shows them live, and offers a
**"lock current"** button that copies the current auto values into manual
settings. `FocusFoM` acts as a **focus meter** for the HQ camera's manual lens.

---

## 4. IPC: Go ↔ helper

### 4.1 What goes over it
| Flow | Volume |
|---|---|
| H.264 video, helper → Go | ≤ 25 Mbit/s ≈ **3 MB/s** worst case, 30–120 messages/s, 5–500 KB each |
| Control requests and replies | a few messages per user action, < 1 KB |
| Events (state, metadata) | ~2–5 messages/s, < 1 KB |
| Stills | **not sent over IPC**: the helper writes files into the captures directory and returns the path |

### 4.2 Options compared

| | stdin/stdout pipes | **Unix domain sockets** | Shared memory |
|---|---|---|---|
| Latency per message | ~10–30 µs | ~10–30 µs | ~1–5 µs, plus a synchronisation mechanism anyway (futex/eventfd/socket) |
| Throughput | GB/s range | GB/s range | memory bandwidth, zero-copy |
| Cost at our load (3 MB/s) | negligible CPU | negligible CPU | negligible — saving copies of 3 MB/s gains nothing |
| Separate channels (control vs video) | awkward (stdin/stdout plus extra fds) | **natural** (two sockets) | needs a side channel for control |
| Process lifecycle | tied to the parent; a Go restart kills the camera | **independent**: either side can restart and reconnect; the helper can run as its own service | independent, but crash cleanup is tricky |
| Stray `print()` or library output | **corrupts the binary stream** | harmless (stdout/stderr are only logs) | harmless |
| Debugging | hard | `socat - UNIX-CONNECT:ctl.sock` and type JSON | needs tools |
| Future zero-copy | no | **yes**: pass memfd/dmabuf fds over the socket (`SCM_RIGHTS`) | — |
| Complexity | lowest | low | highest (ring buffer, ownership, sync, crash recovery) |

All three add a negligible fraction of the ~33 ms frame interval (typical Linux
figures; M1 measures the real IPC latency). **Latency and throughput do not
decide the choice at our load; robustness and simplicity do.**

**Decision: Unix domain sockets (SOCK_STREAM), two connections:**
- `ctl.sock`: JSON lines for requests, replies and events. Human-readable and
  scriptable.
- `video.sock`: length-prefixed binary frames. A large still never delays a
  control reply, because stills don't go over IPC at all.

**Shared memory is deferred** until raw frames need to cross the boundary (for
example future OpenCV processing in Go, or 12 MP YUV at 10 fps ≈ 185 MB/s).
It can then be added **without redesign**: the helper passes a `memfd` ring
buffer's fd over `ctl.sock` with `SCM_RIGHTS`, and slot indexes go through
the existing sockets.

### 4.3 Topology
- **Go listens, the helper connects.** Go creates both sockets (mode 0600) in
  the runtime directory (`$XDG_RUNTIME_DIR/camweb/` or `<data>/run/`), then
  starts the helper with the socket paths as arguments. There is no startup
  race, and a restarted helper simply reconnects.
- `-helper=spawn` (default): Go embeds `camhelper.py` (`embed`), writes it to
  the runtime directory, runs `python3 camhelper.py --ctl … --video …`,
  forwards its stderr to the log, and restarts it with backoff (1 s, 2 s, 4 s …
  30 s).
- `-helper=external`: Go only listens. Useful when the helper runs under
  systemd, while developing it by hand, or when testing a C++ helper.
- On connect, the helper sends `hello` (protocol version, camera info, sensor
  modes, control ranges). Go then sends `configure`. If either socket drops,
  Go treats the session as dead, closes both, and (in spawn mode) kills and
  restarts the helper.

Full message definitions: [`PROTOCOL.md`](PROTOCOL.md).

---

## 5. Streaming to the browser (unchanged from v0.1, simplified)

### 5.1 Protocol choice
**H.264 inside a streamed HTTP response (`fetch` + ReadableStream), decoded
with WebCodecs.**
- Zero dependencies on server and client, low latency (no player buffer),
  internet friendly (plain HTTPS on one port; works through reverse proxies
  and tunnels), and 1080p30 at 4–8 Mbit/s.
- Considered and rejected for v1: MJPEG (20–50 Mbit/s at 1080p), HLS/LL-HLS
  (seconds of latency), MSE/fMP4 (more buffering, needs a muxer), WebRTC
  (best on lossy links, but the large `pion` dependency plus ICE/STUN/TURN; it
  is the upgrade path).
- WebCodecs requires a **secure context**: HTTPS (self-signed cert option,
  your own cert, or a TLS-terminating proxy or tunnel) or `localhost`.

### 5.2 Wire format of `GET /stream`
Body = sequence of messages, each a 16-byte header plus payload:
```
0  u8   type    1 = H.264 AU (Annex-B), 2 = event (JSON), 3 = keepalive
1  u8   flags   bit0 keyframe, bit1 contains SPS/PPS
2  u16  reserved
4  u32  payload length (BE)
8  u64  ts — µs since Unix epoch, taken from the helper's wall_us (when the frame left the encoder)
16 ...  payload
```
Events forwarded to the browser: `state` (streaming / reconfiguring /
capturing / helper-restarting / error), `stream` (width, height, fps,
codec), `metadata` (2 Hz), `drop` (frames dropped for this viewer).

### 5.3 Hub and backpressure
- One encoded stream is fanned out to N viewers. Each viewer has a bounded
  queue (default 30 messages ≈ 1 s) and its own writer goroutine.
- A new viewer, or a new encoder session (`stream_start` from the helper),
  waits for the next keyframe.
- **Drop-to-keyframe**: when a viewer's queue is full, everything is dropped
  for that viewer until the next IDR. Latency stays bounded on slow links.
- The kernel send buffer is kept small (`SetWriteBuffer(256 KiB)` through
  `http.Server.ConnContext`) so backlog lands in our queue, where we can drop
  it. There is a per-write deadline of 5 s, and keepalives every 2 s.

### 5.4 Browser player (`player.js`, vanilla)
- `fetch('/stream')`, then `getReader()`, then parse headers. On an SPS change,
  `VideoDecoder.configure({codec: 'avc1.PPCCLL' from SPS, optimizeForLatency:
  true, hardwareAcceleration: 'prefer-hardware'})`. No `description` is passed,
  so chunks are Annex-B.
- Only the newest decoded frame is drawn, on `requestAnimationFrame`; older
  frames are closed. If `decodeQueueSize > 3` or the estimated latency stays
  over 400 ms, non-key frames are skipped until the next IDR.
- Latency estimate = client now − `ts` − clock offset. The offset comes from
  `/api/time` (NTP-style, the minimum RTT of 5 samples).
- The stream reconnects with backoff, and is aborted while the tab is hidden.

---

## 6. Go server

### 6.1 Config model
Split by **how disruptive a change is**. `camera.Manager` diffs the old and
new config and sends the least disruptive helper command(s):

```go
type Config struct {
    Pipeline Pipeline // needs reconfigure
    Encoder  Encoder  // needs encoder restart
    Controls Controls // live
    Still    Still    // used per capture only
}
type Pipeline struct {
    Profile      string // "4:3" | "16:9" | "fast"
    StreamSize   Size   // must be one of the profile's lores choices
    HFlip, VFlip bool   // rotation 180 = both
    BufferCount  int
    RawSnapshots bool   // include raw stream so snapshots can save DNG
}
type Encoder struct {
    Bitrate int    // bps
    GOP     int    // frames between IDRs
    Profile string // baseline|main|high
}
type Controls struct {
    FPS        float64       // → FrameDurationLimits (d,d)
    AutoExposure bool        // → AeEnable
    ShutterUS  int           // → ExposureTime (manual)
    Gain       float64       // → AnalogueGain (manual)
    EV         float64       // → ExposureValue
    Metering   string        // → AeMeteringMode
    ExposureMode string      // → AeExposureMode
    AutoWB     bool          // → AwbEnable
    AWBMode    string        // → AwbMode
    WBGainR, WBGainB float64 // → ColourGains (manual WB)
    Brightness, Contrast, Saturation, Sharpness float64
    Denoise    string        // → NoiseReductionMode
    Zoom       Rect          // normalised → ScalerCrop (sensor pixels, from ScalerCropMaximum)
}
type Still struct {
    Format  string // jpg|png
    Quality int
    Raw     bool   // DNG
}
```
- Ranges and defaults come from the helper's `hello` (libcamera
  `camera_controls` gives min/max/default for each control), plus Go-side
  sanity limits.
- Mapping to libcamera control names lives **only in Go**
  (`internal/camera/controls.go`).
- The config is saved to `<data>/config.json` and applied after each `hello`
  (so it survives helper and server restarts).

### 6.2 HTTP API
| Method | Path | Notes |
|---|---|---|
| GET | `/` | page (`html/template`) |
| GET | `/static/{file}` | embedded JS/CSS |
| GET | `/stream` | framed H.264 (§5.2) |
| GET | `/api/config` | full `Config` |
| PUT | `/api/config` | partial JSON merged onto the config; reply says what was done: `{"applied": "live"\|"encoder"\|"reconfigure", "config": {...}}` |
| PATCH | `/api/controls` | fast path for sliders: only `Controls` fields, always live |
| POST | `/api/config/reset` | defaults |
| GET | `/api/capabilities` | profiles, control ranges, sensor modes (from `hello`) |
| GET | `/api/status` | state, helper pid/uptime/restarts, viewers, fps, bitrate, CPU temperature, last error, last metadata |
| GET | `/api/time` | clock-offset helper |
| POST | `/api/capture` | `{"mode":"snapshot"\|"full", "format", "quality", "raw"}` → `{name, url, raw_url?, width, height, size, ms}` |
| GET | `/api/captures` | list |
| GET / DELETE | `/captures/{name}` | download / delete (name checked against the directory listing) |

Security: optional Basic auth (`-auth user:pass` / `CAMERA_AUTH`), TLS
(`-tls-cert/-tls-key` or `-tls-self-signed`), `Origin` check on state-changing
methods, strict validation. The helper gets only validated, typed values.

### 6.3 UI
- Live video canvas with stats overlay (fps, kbps, resolution, latency,
  drops) and a state overlay (reconfiguring / capturing / reconnecting).
- **Live controls** (exposure, WB, image, zoom, fps): sliders send `PATCH
  /api/controls` (100 ms debounce). There is no Apply button for these.
- **Pipeline and encoder** settings (profile, stream size, flips, bitrate,
  GOP): an explicit **Apply** button marked "briefly interrupts stream".
- **Readout** of live AE/AWB values from metadata, with **Lock current** and a
  focus meter (`FocusFoM` bar).
- **Snapshot** (no interruption) and **Full-res** (short pause) buttons. A
  gallery with download and delete.
- Collapsible `<details>` sections. Responsive layout for phones.

---

## 7. Repository layout

```
camera_rpi/
├── go.mod                          module github.com/mhbvr/camera_rpi (no requires)
├── cmd/
│   ├── camweb/main.go              server entry point
│   └── fakecam/main.go             fake helper (speaks PROTOCOL.md, plays a .h264 file) for dev/CI
├── helper/
│   ├── camhelper.py                Picamera2 helper (embedded into the Go binary)
│   └── embed.go                    //go:embed camhelper.py
├── internal/
│   ├── proto/                      PROTOCOL.md types: framing, messages (shared by server, fakecam, tests)
│   ├── helper/                     listener, session (ctl RPC + video reader), supervisor (spawn/restart)
│   ├── camera/                     Config, profiles, validation, controls mapping, diff, Manager, persistence
│   ├── stream/                     Hub, per-viewer queues, browser wire format
│   ├── h264/                       NAL type scan, SPS → codec string (small)
│   ├── web/                        server, handlers, middleware, TLS, templates/, static/
│   └── conformance/                protocol conformance tests runnable against any helper
├── deploy/camweb.service
└── docs/{DESIGN.md, PROTOCOL.md, PLAN.md}
```

---

## 8. Latency budget (1080p30, expected; measured in M1)

| Stage | Estimate |
|---|---|
| Sensor exposure and readout | ~33 ms (less at higher fps) |
| ISP and HW H.264 encode | 10–20 ms |
| Helper hand-off (Python callback, queue, socket) | < 1–2 ms (to measure; GIL contention from JPEG encoding is a risk) |
| Go hub and HTTP write | ~1 ms |
| Network | LAN 1–5 ms; internet RTT/2 plus I-frame serialisation |
| Browser HW decode | 5–15 ms |
| Display (next vsync) | 0–16 ms |
| **Total** | **LAN ≈ 60–100 ms**; internet ≈ 120–300 ms depending on link |

---

## 9. Risks

| Risk | Mitigation |
|---|---|
| Python GIL: a JPEG encode of a 12 MP frame (software, ~0.5 s?) stalls the video sender thread and adds jitter | measure in M0; encode in a separate **process** (`multiprocessing`, buffer via shared memory) if needed; or use the HW MJPEG encoder for snapshots |
| lores/main aspect and crop behave differently than assumed | M0 spike; the profile table lives in Go and is easy to adjust |
| The encoder cannot survive `switch_mode` | already planned: explicit stop/start of the encoder |
| CMA exhaustion with full-res plus raw | `buffer_count=2` for stills, raw only on request; document the `cma=` overlay setting |
| Picamera2 API differences between versions (`Output.outputframe` signature, encoder args) | pin a minimum version in `hello`; use `*args, **kwargs` in the output adapter |
| TCP latency build-up over the internet | drop-to-keyframe, small send buffer; WebRTC as the upgrade path |
| WebCodecs needs HTTPS | self-signed cert option, proxy or tunnel docs |
