# camera_rpi — Implementation Plan

This plan implements [`DESIGN.md`](DESIGN.md) v0.2 with the IPC contract in
[`PROTOCOL.md`](PROTOCOL.md). Work is split into milestones. Each ends with
something runnable and has explicit acceptance criteria.

Sizes: **S** ≈ < 0.5 day, **M** ≈ 0.5–1 day, **L** ≈ 1–2 days.

```
M0 Hardware spike (Pi)          ─┐  numbers that fix the open assumptions
M1 IPC + live stream skeleton   ─┤  end-to-end video, helper supervision, fake helper
M2 Configuration + live controls─┤  full protocol, config model, UI
M3 Snapshot                     ─┤  live capture from main → JPEG download
M4 Polish                       ─┘  backpressure tuning, systemd unit, README
(later) C++ helper              — same PROTOCOL.md, validated by the conformance suite
```

---

## Status

| Milestone | Code | Verified here (no Pi) | Needs the Pi |
|---|---|---|---|
| M0 spike | `spike/m0_spike.py` | runs against the fake Picamera2 | **done**, results and analysis in `docs/SPIKE.md`. Fixes applied: bitrate compensation (the encoder assumes 30 fps), absolute timestamps; camera-side latency 53 ms p50 measured. Compensation confirmed (spike 0.9 re-run). Follow-ups: check aspect images, glass-to-glass |
| M1 IPC + stream | done | Go tests (race detector), fakecam + headless Chromium decodes and renders 1440×1080@30; helper `kill -9` → recovers in ~1 s | glass-to-glass latency, CPU |
| M2 config + live controls | done | Go tests; real `camhelper.py` on fake Picamera2: live controls, encoder restart, reconfigure | that controls take effect on the real camera (auto/manual exposure semantics, M0 0.7) |
| M3 snapshot | done | 2028×1520 JPEG via both the Go fake and `camhelper.py` (Pillow and simplejpeg paths, colour round-trip test) | no video gap during capture (M0 0.4/0.5) |
| M4 polish | systemd unit, README, HTTPS (`make cert`), MJPEG fallback | MJPEG fallback with the real helper on the fake Picamera2 (headless Chromium without WebCodecs, and with a stubbed VideoDecoder that fails like Firefox for Android) | backpressure tuning over a real link; spike 0.10 done (no H.264 drops, +5 % CPU; MJPEG bitrate compensation within ±10 %); MJPEG fallback on a real Firefox for Android (runtime decoder-failure detection) |

Additions not in the original plan: `camhelper/fake/` (a fake Picamera2 so the
real helper runs in CI) and `camhelper/test_camhelper.py`.

---

## M0 — Hardware spike (on the Pi 4 + HQ camera) — M

Goal: check the assumptions in DESIGN.md before writing production code.
Throwaway scripts go in `spike/` (not part of the build). Results are written
to `docs/SPIKE.md`.

| # | Experiment | Measure / decide |
|---|---|---|
| 0.1 | `picam2.sensor_modes`, `camera_controls`, `camera_properties` dump | Real mode list and control ranges → feeds the Go constants and `hello` |
| 0.2 | Video config main 2028×1520 + lores 1440×1080 YUV420, `H264Encoder` on lores, custom `Output.outputframe` | Is each call **exactly one AU**? Is the keyframe flag correct? Are SPS/PPS present with `repeat=True`? What does the `timestamp` argument mean (µs, clock domain vs `SensorTimestamp` and `time.monotonic_ns()`)? The `outputframe` signature on the installed version. CPU % at 30 fps |
| 0.3 | Capture-to-hand-off latency | `time.monotonic_ns() - SensorTimestamp` at `outputframe` (if same clock domain), averaged over 300 frames |
| 0.4 | `capture_request()` + copy main while encoding | Any gaps in encoder frame timestamps (no dropped frames)? Copy time for 2028×1520 YUV420 |
| 0.5 | JPEG encode of a 2028×1520 YUV420 buffer in a worker thread | Encode time. **Jitter of video `outputframe` intervals during the encode** (GIL impact). If jitter > 10 ms → plan to encode in a subprocess |
| 0.6 | Stream sizes in the 2028×1520 mode: lores 1440×1080, 1024×768, 640×480 | Visual check (circle target) → no stretch? Final sizes after `align_configuration` |
| 0.7 | `set_controls` latency | Frames until `ExposureTime` metadata reflects the change; FrameDurationLimits change at runtime works within a mode |
| 0.8 | Unix socket hand-off | Python `sendall` of 200 KB frames to a local reader: per-message latency (expected tens of µs) |
| 0.9 | Encoder limits | 1440×1080@40 at 2–12 Mbit/s stable? |

**Exit criteria:** `docs/SPIKE.md` holds numbers for each row. DESIGN.md
sizes and risks are updated. Decision recorded: JPEG encode in a thread or
in a subprocess.

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
   - JSON-RPC 2.0 types (about 100 lines, no library):
     `Request{JSONRPC, ID, Method, Params json.RawMessage}`,
     `Response{JSONRPC, ID, Result json.RawMessage, Error *RPCError}`,
     `Notification{JSONRPC, Method, Params}`, `RPCError{Code, Message, Data}`
     plus the error-code constants from PROTOCOL §3.1. Decoding an incoming
     message tells a response (has `id` and `result`/`error`) from a
     notification (has `method`, no `id`).
   - Typed params/results: `Hello`, `ConfigureParams`, `SnapshotParams`,
     `StreamStart`, … mirroring PROTOCOL.md.
   - Line reader with a 1 MiB limit (`bufio.Reader` + manual limit).
   - Tests: round-trip encode/decode, truncated input, oversize rejection,
     error objects, response vs notification classification.
3. **`internal/helper`** (L)
   - `Listener`: creates the runtime dir, removes stale sockets, listens on
     `ctl.sock` and `video.sock` (chmod 0600), and accepts pairs → `Session`.
   - `Session`:
     - `Call(ctx, method string, params any, result any) error`: assigns the
       id, writes the JSON-RPC request line under a mutex, waits on a per-id
       channel, and turns an `error` object into a Go `*proto.RPCError`.
       Context timeout.
     - reader goroutine for ctl: routes responses by id and notifications to
       `Notifications() <-chan proto.Notification`.
     - reader goroutine for video: `Video() <-chan proto.VideoMsg`, or a
       callback into the hub (avoids one copy).
     - `Done() <-chan struct{}`, `Err()`: closing either connection ends the
       session.
   - `Supervisor` (spawn mode): writes the embedded `camhelper.py` to the
     runtime dir, `exec.Command("python3", script, "--ctl", …, "--video", …)`,
     pipes stderr into `log` with prefix `[helper]`, restarts on exit with
     backoff of 1 s→30 s (reset after 60 s healthy), sends `shutdown` and
     SIGTERM/SIGKILL on server stop. External mode: no spawning.
   - Tests use an in-process fake helper (goroutine speaking the protocol over
     real sockets in `t.TempDir()`).
4. **`camhelper/camhelper.py` v1** (L): see "Helper internals" below.
   JSON-RPC methods: `configure` (fixed or param-driven), `stop`, `shutdown`;
   notifications: `hello`, `state`, `stats`; video: `stream_start`, AUs.
5. **`cmd/fakecam`** (M): Go implementation of the helper side for dev/CI.
   - Reads a `.h264` Annex-B file (needs a small AU splitter: `internal/h264`
     `SplitAnnexB` — used **only** by fakecam), loops it at the configured fps,
     sends `hello` with an IMX477-like fake mode and controls, and accepts all
     commands (`snapshot` writes a generated JPEG via `image/jpeg`).
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
   `state` notifications to the hub (as browser events).
9. **`internal/web` (minimal)** (M): `GET /`, `/static/*` (embed), `GET /stream`
   (subscribe, set headers, write loop with `http.ResponseController`
   `Flush` + `SetWriteDeadline`). Small `SetWriteBuffer` via `ConnContext`.
10. **Frontend player** (M): `static/player.js` (fetch reader, parser,
    VideoDecoder, rAF render, newest-frame-only, reconnect every 2 s),
    `static/app.js` (stats overlay: fps, kbps, resolution, drops),
    `templates/index.html`, `static/style.css`.
11. **`cmd/camweb/main.go`** (S): flags (`-listen`, `-data`, `-runtime-dir`,
    `-helper spawn|external`, `-helper-cmd`, `-python`, `-hflip`, `-vflip`),
    wiring, graceful shutdown on SIGINT/SIGTERM.

### Helper internals (`camhelper.py`, target ≤ 500 lines, stdlib + Picamera2 only)
```
main()
 ├─ parse args, logging → stderr
 ├─ connect ctl, video (retry 10 s)
 ├─ cam = Camera()                      # wraps Picamera2, holds cam_lock
 ├─ send hello
 └─ RpcLoop (main thread): read lines → parse JSON-RPC 2.0 → dispatch table {method: handler}
       quick methods (set_controls)      → handled inline
       camera methods (configure, snapshot, set_encoder, stop)
                                         → worker thread, guarded by cam_lock (-32001 busy if held)
       reply(id, result) / reply_error(id, code, message, data) / notify(method, params)
                                         # one write lock for ctl.sock
SocketOutput(picamera2.outputs.Output)
   outputframe(frame, keyframe=True, timestamp=None, *a, **kw)
       → video_sock.sendall(pack_header(...) + frame)   # direct write, no queue
post_callback(request)                   # metadata sampler, rate-limited to metadata_hz
StillWorker                              # thread (or subprocess if M0 says so): encode JPEG from the copied main buffer
```
- Every exception in a command → error response; unhandled exception in a
  thread → `error` notification and exit(1) (the supervisor restarts).
  Errors map to the JSON-RPC codes in PROTOCOL §3.1.
- SIGTERM → close the camera cleanly.
- No global mutable state outside `Camera`. Wire framing is in one function
  (`pack_header`), mirroring `internal/proto`.

### Acceptance criteria
- [ ] `go test ./...` passes; `go vet` is clean; the binary builds for
  `linux/arm64` with `CGO_ENABLED=0`.
- [ ] With `fakecam`: open `http://localhost:8080` and the test pattern
  plays; killing fakecam shows "reconnecting"; restarting
  it resumes video.
- [ ] On the Pi: 1440×1080@30 plays in Chrome on the LAN (via SSH forward or
  the insecure-origin flag, see DESIGN §5.1). **Glass-to-glass latency
  measured** (camera pointed at an on-screen ms clock) and recorded in
  `docs/SPIKE.md`. Target ≤ 150 ms.
- [ ] `kill -9` of the helper → stream recovers by itself in < 5 s.
- [ ] Three browser tabs at once; CPU usage of camweb and the helper recorded.

---

## M2 — Configuration + live controls — L

### Tasks
1. **Config model** (`internal/camera/config.go`) (M): structs from DESIGN
   §6.1, `Defaults()`, `Validate(caps) error` (ranges from `hello` plus
   Go-side limits; stream size one of 1440×1080 / 1024×768 / 640×480), `Merge(partial json)`, JSON persistence
   (`<data>/config.json`, atomic write via temp + rename).
2. **Configure args** (S): constants from DESIGN §3.1 (M0 numbers) + config
   + flip flags → `ConfigureArgs` (sensor, main, lores, transform,
   FrameDurationLimits, encoder with fixed `high` profile).
3. **Control mapping** (`controls.go`) (M): `Controls` → libcamera control map
   (enum names → ints using `hello` values; fps → `FrameDurationLimits`;
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
5. **Helper commands** (M): `configure` fully arg-driven, `set_controls`
   (whitelist = names in `camera_controls`), `set_encoder`, `metadata`
   notifications
   via `post_callback`.
6. **HTTP API** (S): `GET /api/config`, `POST /api/config` (partial, the
   only settings endpoint), `POST /api/config/reset`; JSON errors
   `{"error": "...", "field": "..."}`.
7. **UI** (L): a settings form with hard-coded fields and ranges (rendered by
   the template from Go constants), in plain JS:
   - every input sends `POST /api/config` on its `change` event (no debounce,
     no Apply button); fields that briefly interrupt the stream (stream size,
     bitrate, GOP) are labelled;
   - sections: Stream (size, fps, bitrate, GOP), Exposure (auto/manual toggle
     that disables manual inputs, logarithmic shutter slider, gain, EV,
     metering, mode), White balance, Image (brightness, contrast, saturation,
     sharpness, denoise);
   - actual values (shutter, analogue/digital gain, frame time, AE state,
     lux, CT) with an auto-exposure limit warning, **Lock current**, the raw
     `FocusFoM` value, and ⓘ info popups (DESIGN §3.5, §3.6).

### Acceptance criteria
- [ ] Moving exposure, gain, WB and contrast sliders changes the image
  with **no stream interruption** (no `stream_start` in the log).
- [ ] Bitrate change → new encoder session; the gap is logged and below
  200 ms (or the M0 number).
- [ ] Stream size change (1440×1080 ↔ 1024×768 ↔ 640×480) works; the
  decoder reconfigures automatically in the browser.
- [ ] Invalid values are rejected with a clear error; the config survives
  server and helper restarts.
- [ ] Unit tests: validation, merge, diff → ApplyKind, control mapping.

---

## M3 — Snapshot (live capture) — M

### Tasks
1. **Helper** (M): `snapshot`: `capture_request()` → copy `main` → `release()`
   → worker encodes JPEG (thread or subprocess per M0) → `.tmp` + rename →
   respond with file info and `timing_ms`.
2. **Manager** (S): `Snapshot(quality) (path, info, error)`: generate the name
   `IMG_YYYYMMDD_HHMMSS[_n]` in `<data>/captures/`, call the helper with a 10 s
   timeout.
3. **HTTP** (S): `POST /api/capture` → `http.ServeContent` of the new file with
   `Content-Disposition: attachment` and `X-Capture-Width/Height`.
4. **UI** (S): a **Capture** button (shortcut `c`) → `fetch` POST → blob →
   trigger download (`<a download>`); show the JPEG quality setting; a brief
   "captured 2028×1520, 1.8 MB" toast.

### Acceptance criteria
- [ ] Capture during streaming: **no gap** in the video (seq/pts continuous
  in the helper log); the JPEG has the main-stream resolution of the active
  stream (2028×1520).
- [ ] Snapshot exposure and colour match the live view.
- [ ] Twenty captures in a row: no helper restarts, no visible stutter in the
  stream (or stutter within the M0 jitter numbers).

---

## M4 — Polish — M

1. **Backpressure tuning** (M): Chrome DevTools throttling (3G/4G profiles)
   and `tc netem` on the Pi (delay, loss). Tune the hub queue size and the
   send buffer. Write down the results.
2. **Deploy** (S): `deploy/camweb.service` (User=pi, `video` group,
   `RuntimeDirectory=camweb`, Restart=on-failure) and install steps.
3. **README** (S): quick start, flags, how to get a secure context (SSH
   forward / browser flag / Tailscale / Cloudflare Tunnel), troubleshooting.

### Acceptance criteria
- [ ] Through an SSH forward or tunnel over the internet: 1080p30 at
  5 Mbit/s stable for 30 min; glass-to-glass latency at the end is the same
  as at the start.
- [ ] With 2 Mbit/s throttling and a 6 Mbit/s stream: latency stays bounded
  (< 1.5 s) thanks to drops, and recovers when the throttle is removed.

---

## Later (not in this phase)
- Full-resolution 12 MP capture (`capture_full`, PROTOCOL §6) and DNG raw.
- Gallery / capture management, TLS and auth in the app.
- **Protocol conformance suite** (`internal/conformance`): Go tests that drive
  any helper (`HELPER_CMD`) through PROTOCOL.md. Needed before the C++ helper.
- **C++ helper**: implements PROTOCOL.md v1 directly on libcamera plus the
  V4L2 M2M H.264 encoder and libjpeg-turbo. Accepted when it passes the
  conformance suite, with zero changes to the Go server.
- Shared memory frame sharing (PROTOCOL §6) if raw-frame processing is wanted.

---

## Testing strategy summary
| Layer | How |
|---|---|
| `proto`, `h264`, `stream`, `camera` (config/diff/mapping) | Go unit tests, table-driven, no hardware |
| `helper` (Go side) | tests with an in-process fake helper over real Unix sockets |
| end-to-end without a Pi | `fakecam` + camweb + headless Chromium (Playwright script in `spike/` that checks frames are decoded on the canvas) |
| helper (Python) | end-to-end on the Pi; M0 spike scripts kept as manual benchmarks |
| performance and latency | glass-to-glass photo, stats overlay, helper `timing_ms`, `tc netem` / DevTools throttling |

## Dependencies (final)
- Go: **stdlib only**.
- Python: system `python3`, `python3-picamera2` (with numpy and its JPEG
  helpers), **no pip installs**.
- Browser: no libraries.
- Dev only: `ffmpeg` to generate test streams (optional; a small test file is
  committed).
