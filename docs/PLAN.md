# camera_rpi — Implementation Plan

This plan implements [`DESIGN.md`](DESIGN.md) v0.2 with the IPC contract in
[`PROTOCOL.md`](PROTOCOL.md). Work is split into milestones. Each ends with
something runnable and has explicit acceptance criteria.

Sizes: **S** ≈ < 0.5 day, **M** ≈ 0.5–1 day, **L** ≈ 1–2 days.

```
M0 Hardware spike (Pi)          ─┐  numbers that fix the open assumptions
M1 IPC + live stream skeleton   ─┤  end-to-end video, helper supervision, fake helper
M2 Configuration + live controls─┤  full protocol, config model, UI
M3 Stills                       ─┤  snapshot, full-res, DNG, gallery
M4 Hardening                    ─┘  TLS, auth, backpressure tuning, deploy, docs
(later) C++ helper              — same PROTOCOL.md, validated by the conformance suite
```

---

## M0 — Hardware spike (on the Pi 4 + HQ camera) — M

Goal: check the assumptions in DESIGN.md before writing production code.
Throwaway scripts go in `spike/` (not part of the build). Results are written
to `docs/SPIKE.md`.

| # | Experiment | Measure / decide |
|---|---|---|
| 0.1 | `picam2.sensor_modes`, `camera_controls`, `camera_properties` dump | Real mode list and control ranges → feeds the profile table and `hello` |
| 0.2 | Video config main 2028×1520 + lores 1440×1080 YUV420, `H264Encoder` on lores, custom `Output.outputframe` | Is each call **exactly one AU**? Is the keyframe flag correct? Are SPS/PPS present with `repeat=True`? What does the `timestamp` argument mean (µs, clock domain vs `SensorTimestamp` and `time.monotonic_ns()`)? The `outputframe` signature on the installed version. CPU % at 30 fps |
| 0.3 | Capture-to-hand-off latency | `time.monotonic_ns() - SensorTimestamp` at `outputframe` (if same clock domain), averaged over 300 frames |
| 0.4 | `capture_request()` + copy main while encoding | Any gaps in encoder frame timestamps (no dropped frames)? Copy time for 2028×1520 YUV420 |
| 0.5 | JPEG encode of a 2028×1520 and a 4056×3040 YUV420 buffer in a worker thread | Encode time. **Jitter of video `outputframe` intervals during the encode** (GIL impact). If jitter > 10 ms → plan to encode in a subprocess |
| 0.6 | `stop_encoder` → `switch_mode(full-res still cfg, buffer_count=2)` → capture → `switch_mode(video)` → `start_encoder` | Video gap (last AU before to first AU after). Does exposure/gain/colour gains metadata before and after match (AE/AWB carried over)? Can the encoder stay attached during `switch_mode` (try both)? |
| 0.7 | Same with `raw` in the still config + DNG save | CMA ok? (`/proc/meminfo` CmaFree before and during.) DNG save time |
| 0.8 | Aspect and crop: profile `16:9` (mode 2028×1080, main 1920×1080, lores 1280×720) and `fast` | `ScalerCrop` metadata and a visual check (circle target) → no stretch? Final sizes after `align_configuration` |
| 0.9 | `set_controls` latency | Frames until `ExposureTime` metadata reflects the change; FrameDurationLimits change at runtime works within a mode |
| 0.10 | Unix socket hand-off | Python `sendall` of 200 KB frames to a local reader: per-message latency (expected tens of µs) |
| 0.11 | Encoder bitrate/profile limits | 1440×1080@40, 1920×1080@50 (`16:9`), 1280×960@120 (`fast`) stable? |

**Exit criteria:** `docs/SPIKE.md` holds numbers for each row. DESIGN.md
profiles and risks are updated. Decisions recorded: (a) JPEG encode in a thread
or a subprocess; (b) encoder kept or restarted around `switch_mode`; (c)
whether a still AE lock-in is needed.

---

## M1 — IPC + live stream skeleton — L

Goal: browser shows live H.264 from the Pi with measured latency. The helper is
spawned, supervised and auto-restarted. Everything also runs without a Pi using
`fakecam`.

### Tasks
1. **Repo skeleton** (S): `go.mod` (`github.com/mhbvr/camera_rpi`, Go 1.22,
   no requires), directory layout from DESIGN §7, `Makefile` with `build`,
   `build-arm64`, `test`, `vet`, and a `.gitignore`.
2. **`internal/proto`** (M): shared protocol types.
   - `VideoHeader` (32 B) encode/decode, `ReadVideoMsg(r) (Msg, error)` with
     reuse of a pooled buffer, max payload guard (8 MiB).
   - Ctl types: `Request{ID, Cmd, Args json.RawMessage}`,
     `Response{ID, OK, Result, Error}`, `Event{Event, Data}`, `Hello`,
     `ConfigureArgs`, `StreamStart`, … mirroring PROTOCOL.md.
   - Line reader with a 1 MiB limit (`bufio.Reader` + manual limit).
   - Tests: round-trip encode/decode, truncated input, oversize rejection.
3. **`internal/helper`** (L)
   - `Listener`: creates the runtime dir, removes stale sockets, listens on
     `ctl.sock` and `video.sock` (chmod 0600), and accepts pairs → `Session`.
   - `Session`:
     - `Call(ctx, cmd string, args any, result any) error`: assigns the id,
       writes the line under a mutex, waits on a per-id channel. Context
       timeout.
     - reader goroutine for ctl: routes responses by id and events to
       `Events() <-chan proto.Event`.
     - reader goroutine for video: `Video() <-chan proto.VideoMsg`, or a
       callback into the hub (avoids one copy).
     - `Done() <-chan struct{}`, `Err()`: closing either connection ends the
       session.
     - pinger: every 5 s; 3 failures → close.
   - `Supervisor` (spawn mode): writes the embedded `camhelper.py` to the
     runtime dir, `exec.Command("python3", script, "--ctl", …, "--video", …)`,
     pipes stderr into `log` with prefix `[helper]`, restarts on exit with
     backoff of 1 s→30 s (reset after 60 s healthy), sends `shutdown` and
     SIGTERM/SIGKILL on server stop. External mode: no spawning.
   - Tests use an in-process fake helper (goroutine speaking the protocol over
     real sockets in `t.TempDir()`).
4. **`helper/camhelper.py` v1** (L): see "Helper internals" below.
   Commands: `hello`, `configure` (fixed or arg-driven), `ping`, `stop`,
   `shutdown`; events: `state`, `stats`; video: `stream_start`, AUs.
5. **`cmd/fakecam`** (M): Go implementation of the helper side for dev/CI.
   - Reads a `.h264` Annex-B file (needs a small AU splitter: `internal/h264`
     `SplitAnnexB` — used **only** by fakecam), loops it at the configured fps,
     sends `hello` with IMX477-like fake modes and controls, and accepts all
     commands (stills: writes a generated JPEG via `image/jpeg`).
   - Test asset generation documented: `ffmpeg -f lavfi -i testsrc2=size=1440x1080:rate=30 -c:v libx264 -profile:v high -bf 0 -g 30 -x264-params repeat-headers=1 testdata/test_1440x1080.h264`
     (a small file is committed in `testdata/`; the generator script is in `spike/`).
6. **`internal/h264`** (S): `NALTypes(au) []uint8`, `FindSPS(au) []byte`,
   `CodecString(sps) string` ("avc1.%02X%02X%02X"). Tests with real SPS bytes.
7. **`internal/stream`** (M)
   - `Hub`: `Publish(msg)`, `Subscribe() *Sub`, `Unsubscribe`; per-sub bounded
     queue (cap 30), `waitKey` state, drop-to-keyframe, drop counter, and
     `ResetSession()` on `stream_start`.
   - Browser framing (16-byte header, DESIGN §5.2) and keepalive ticker.
   - Tests: a slow subscriber gets drops until an IDR and never blocks
     Publish; a new subscriber starts at a key; a session reset.
8. **`internal/camera` (minimal)** (S): `Manager` that, on every new session,
   sends a hard-coded default `configure`, and forwards video to the hub and
   state events to the hub (as browser events).
9. **`internal/web` (minimal)** (M): `GET /`, `/static/*` (embed), `GET /stream`
   (subscribe, set headers, write loop with `http.ResponseController`
   `Flush` + `SetWriteDeadline`), `GET /api/status`, `GET /api/time`. Small
   `SetWriteBuffer` via `ConnContext`.
10. **Frontend player** (M): `static/player.js` (fetch reader, parser,
    VideoDecoder, rAF render, newest-frame-only, reconnect, visibility
    handling), `static/app.js` (stats overlay, latency via `/api/time`
    offset), `templates/index.html`, `static/style.css`.
11. **`cmd/camweb/main.go`** (S): flags (`-listen`, `-data`, `-runtime-dir`,
    `-helper spawn|external`, `-helper-cmd`, `-python`), wiring, graceful
    shutdown on SIGINT/SIGTERM.

### Helper internals (`camhelper.py`, target ≤ 500 lines, stdlib + Picamera2 only)
```
main()
 ├─ parse args, logging → stderr
 ├─ connect ctl, video (retry 10 s)
 ├─ cam = Camera()                      # wraps Picamera2, holds cam_lock
 ├─ VideoSender(video_sock)             # thread: deque(maxlen-managed) → sendall(header+payload)
 ├─ send hello
 └─ CtlLoop (main thread): read lines → dispatch
       quick cmds (ping, set_controls)   → handled inline
       camera cmds (configure, snapshot, capture_full, set_encoder, stop)
                                         → worker thread, guarded by cam_lock (busy if held)
SocketOutput(picamera2.outputs.Output)
   outputframe(frame, keyframe=True, timestamp=None, *a, **kw)
       → VideoSender.put(AU(bytes(frame), key, ts, wall_us=time.time_ns()//1000))
post_callback(request)                   # metadata sampler, rate-limited to metadata_hz
StillWorker                              # thread (or subprocess if M0 says so): encode JPEG/PNG/DNG from copied buffers
```
- Every exception in a command → error response; unhandled exception in a
  thread → `error` event and exit(1) (the supervisor restarts).
- SIGTERM → close the camera cleanly.
- No global mutable state outside `Camera` and `VideoSender`. Wire framing is
  in one function (`pack_header`), mirroring `internal/proto`.

### Acceptance criteria
- [ ] `go test ./...` passes; `go vet` is clean; the binary builds for
  `linux/arm64` with `CGO_ENABLED=0`.
- [ ] With `fakecam`: open `https://localhost:8080` (or `http://localhost`)
  and the test pattern plays; killing fakecam shows "reconnecting"; restarting
  it resumes video.
- [ ] On the Pi: 1440×1080@30 plays in Chrome on the LAN. The stats overlay
  shows the latency estimate. **Glass-to-glass measured** (camera pointed at an
  on-screen ms clock) and recorded in `docs/SPIKE.md`. Target ≤ 150 ms.
- [ ] `kill -9` of the helper → stream recovers by itself in < 5 s.
- [ ] Three browser tabs at once; CPU usage of camweb and the helper recorded.

---

## M2 — Configuration + live controls — L

### Tasks
1. **Config model** (`internal/camera/config.go`) (M): structs from DESIGN
   §6.1, `Defaults()`, `Validate(caps) error` (ranges from `hello` plus
   Go-side limits), `Merge(partial json)`, JSON persistence
   (`<data>/config.json`, atomic write via temp + rename).
2. **Profiles** (`profiles.go`) (S): the table from DESIGN §3.1 using M0
   numbers; profile → `ConfigureArgs` (sensor, main, lores, FrameDurationLimits).
3. **Control mapping** (`controls.go`) (M): `Controls` → libcamera control map
   (enum names → ints using `hello` values; normalised zoom → `ScalerCrop`
   from `scaler_crop_max`, keeping aspect; fps → `FrameDurationLimits`;
   manual vs auto exposure and WB). Table-driven tests.
4. **Diff and apply** (`manager.go`) (M): `Apply(new Config) (ApplyKind, error)`
   - Pipeline differs → `configure` (includes encoder and controls).
   - Else encoder differs → `set_encoder`.
   - Else controls differ → `set_controls` with only the changed keys.
   - Persist **after** helper success; on failure keep the old config and
     return the error.
   - On a new helper session: send the full `configure` from the persisted
     config.
   - Manager serialises Apply/Capture calls with a mutex (helper `busy` should
     never happen).
5. **Capabilities** (S): build `/api/capabilities` from `hello` plus profiles.
6. **Helper commands** (M): `configure` fully arg-driven, `set_controls`
   (whitelist = names in `camera_controls`), `set_encoder`, `metadata` events
   via `post_callback`.
7. **HTTP API** (M): `GET/PUT /api/config`, `PATCH /api/controls`,
   `POST /api/config/reset`, `GET /api/capabilities`; JSON errors
   `{"error": "...", "field": "..."}`.
8. **UI** (L): settings panel generated from `/api/capabilities` (ranges,
   enums) in plain JS:
   - live sections (Exposure, WB, Image, Zoom, FPS) → `PATCH /api/controls`
     with a 100 ms debounce; a logarithmic shutter slider; an auto/manual
     toggle that disables the manual inputs;
   - Pipeline/Encoder sections with an **Apply** button ("interrupts stream
     briefly"), presets (LAN HQ / Internet / Mobile / Low-latency);
   - metadata readout (shutter as 1/x s, gain, CT, lux), **Lock current**,
     and a `FocusFoM` bar with a peak-hold marker;
   - "1:1 zoom" focus-assist button (centre crop at native sensor resolution).

### Acceptance criteria
- [ ] Moving exposure, gain, WB, contrast and zoom sliders changes the image
  with **no stream interruption** (no `stream_start` in the log).
- [ ] Bitrate change → new encoder session; the gap is logged and below
  200 ms (or the M0 number).
- [ ] Profile change (4:3 ↔ 16:9 ↔ fast) works; the decoder reconfigures
  automatically in the browser.
- [ ] Invalid values are rejected with a clear error; the config survives
  server and helper restarts.
- [ ] Unit tests: validation, merge, diff → ApplyKind, control mapping
  (including zoom math).

---

## M3 — Stills — M/L

### Tasks
1. **Helper** (L): `snapshot` (capture_request → copy main [+raw] → release →
   StillWorker encode → tmp + rename), `capture_full` (sequence from DESIGN
   §3.3 using the M0 decisions; `stream_end`/`stream_start` around it),
   `timing_ms` in responses.
2. **Manager** (S): `Capture(mode, params)`: generate the name
   `IMG_YYYYMMDD_HHMMSS[_n]`, call the helper with a timeout
   (snapshot 10 s; full 30 s + shutter time), and send `capturing` /
   `streaming` events.
3. **Captures store** (S): `internal/camera/captures.go`: list (name, size,
   mtime, siblings like `.dng`), delete (both files), name validation
   (`^IMG_[0-9_]+\.(jpg|png|dng)$`).
4. **HTTP** (S): `POST /api/capture`, `GET /api/captures`,
   `GET/DELETE /captures/{name}` (`http.ServeContent`, `Content-Disposition`).
5. **UI** (M): Snapshot and Full-res buttons (with the shortcut `s`), Still
   settings (format, quality, raw), a progress overlay during full-res, and a
   gallery (lazy thumbnails using the image itself, download, delete, a DNG
   badge).
6. **"Raw on snapshot"** (S): pipeline flag → `configure.raw = true`; the UI
   explains the memory cost.

### Acceptance criteria
- [ ] Snapshot during streaming: **no gap** in the video (seq/pts continuous
  in the helper log); the JPEG is 2028×1520 (4:3 profile).
- [ ] Full-res: 4056×3040 JPEG (+ DNG when selected, which opens in
  RawTherapee/darktable); the video gap is logged and matches M0; the stream
  resumes on its own.
- [ ] Exposure of a full-res still visually matches the preview (auto and
  manual modes).
- [ ] Ten snapshots in a row: no CMA errors, no helper restarts.

---

## M4 — Hardening — M/L

1. **TLS** (M): `-tls-cert/-tls-key`; `-tls-self-signed` generates an ECDSA
   P-256 cert (SAN: hostname, `*.local` name, all interface IPs) into
   `<data>/tls/`, valid 1 year, reused across runs.
2. **Auth** (S): Basic auth middleware (`-auth` / `CAMERA_AUTH`), constant-time
   compare, applied to everything including `/stream` and `/captures/`. Refuse
   to start with auth over plain HTTP on a non-loopback address unless
   `-insecure` is given.
3. **CSRF** (S): `Origin`/`Sec-Fetch-Site` check on PUT/PATCH/POST/DELETE.
4. **Backpressure tuning** (M): test with Chrome DevTools throttling (3G/4G
   profiles) and `tc netem` on the Pi (delay, loss). Tune the queue size, send
   buffer and client skip thresholds. Document the results.
5. **Status page details** (S): helper pid, uptime, restarts, versions,
   CPU temperature (`/sys/class/thermal/thermal_zone0/temp`), throttling
   (`/sys/devices/platform/soc/soc:firmware/get_throttled` if available).
6. **Deploy** (S): `deploy/camweb.service` (User=pi, `video` group,
   `RuntimeDirectory=camweb`, Restart=on-failure), install steps in the README,
   and CMA notes (`dtoverlay=vc4-kms-v3d,cma-512` if full-res plus raw fails).
7. **README** (S): quick start, flags, HTTPS options (self-signed /
   Caddy / Cloudflare Tunnel / Tailscale), troubleshooting.
8. **Conformance suite** (M): `internal/conformance`: Go tests that start a
   helper command (env `HELPER_CMD`), act as the server, and check the
   PROTOCOL.md rules: hello first, configure → stream_start → IDR first,
   AUs are whole Annex-B frames, set_controls ok/invalid, snapshot files appear
   atomically, capture_full resumes video before replying, out-of-order
   responses, ping. Runs against `fakecam` in CI and against `camhelper.py`
   on the Pi.

### Acceptance criteria
- [ ] Reachable over the internet through a tunnel with auth: 1080p30 at
  5 Mbit/s stable for 30 min, latency does not creep (stats overlay).
- [ ] With 2 Mbit/s throttling and a 6 Mbit/s stream: latency stays bounded
  (< 1.5 s) thanks to drops, and recovers when the throttle is removed.
- [ ] The conformance suite passes against both fakecam and camhelper.py.

---

## Later: C++ helper (not in this phase)
- Implements PROTOCOL.md v1 directly on libcamera plus the V4L2 M2M H.264
  encoder (`/dev/video11`), and libjpeg-turbo / a DNG writer for stills.
- Can start with `features: ["snapshot","metadata"]` and add `capture_full`
  later; the Go server hides unsupported buttons based on `features`.
- Accepted when it passes `internal/conformance`. Zero changes to the Go
  server expected.
- This is also when shared memory (`share_frames`, PROTOCOL §6) becomes
  interesting, if raw-frame processing is wanted.

---

## Testing strategy summary
| Layer | How |
|---|---|
| `proto`, `h264`, `stream`, `camera` (config/diff/mapping/captures) | Go unit tests, table-driven, no hardware |
| `helper` (Go side) | tests with an in-process fake helper over real Unix sockets |
| end-to-end without a Pi | `fakecam` + camweb + headless Chromium (Playwright script in `spike/` that checks frames are decoded on the canvas) |
| helper (Python) | conformance suite on the Pi; M0 spike scripts kept as manual benchmarks |
| performance and latency | glass-to-glass photo, stats overlay, helper `timing_ms`, `tc netem` / DevTools throttling |

## Dependencies (final)
- Go: **stdlib only**.
- Python: system `python3`, `python3-picamera2` (with numpy and its JPEG/DNG
  helpers), **no pip installs**.
- Browser: no libraries.
- Dev only: `ffmpeg` to generate test streams (optional; a small test file is
  committed).
