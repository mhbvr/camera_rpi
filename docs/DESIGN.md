# camera_rpi — Design Document (v0.5, for review)

A web application for a Raspberry Pi 4 with the HQ Camera (Sony IMX477):

1. a **low-latency live stream** in the browser, usable on the LAN and over the
   internet at HQ (1080p) with reasonable bandwidth;
2. **camera settings** (exposure, gain, white balance, image tuning, frame
   rate, stream resolution, bitrate…), applied **live** where the hardware
   allows it;
3. **still capture without stopping the stream**: a JPEG snapshot from the
   camera's higher-resolution `main` stream (2028×1520, 3.1 MP),
   downloaded straight to the browser.

It is a prototype: keep it simple, keep dependencies few, and make the camera
part replaceable (the Python helper is expected to become C++ later).

**Out of scope at this stage** (the design keeps them possible, see
PROTOCOL §6): TLS/HTTPS in the app, authentication, gallery and file
management, full-resolution 12 MP capture (mode switch), DNG raw, recording to
file, MJPEG fallback, WebRTC, image processing, 16:9 and high-fps sensor
modes, digital zoom, H.264 profile selection, flips in the UI, presets, status
and capabilities endpoints, latency estimation in the browser.

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
- Stills: only a **snapshot without interrupting the stream** (main stream,
  ~3 MP). Full-res capture, TLS, auth and the gallery were dropped in the
  v0.3 review.
- v0.4 review: a single 4:3 sensor mode, no zoom, fixed H.264 profile, flips
  only as startup flags, no helper-side video queue, no ping, one config
  endpoint, no capabilities/status/time endpoints, no presets, and a simpler
  browser player (no latency estimate, no client-side frame skipping, no
  pausing in hidden tabs, fixed-interval reconnect).
- v0.5 review: the control channel uses **JSON-RPC 2.0** (standard spec, no
  library). The video channel keeps its length-prefixed frames, and the browser
  stream keeps `fetch` with the 16-byte header (both decided after looking at
  existing standards, §4.2 and §5.1).
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
  | 1332×990 12-bit | 101.7 | high fps, cropped FOV |
  | 2028×1080 12-bit | 62.8 | 16:9, cropped FOV |
  | 2028×1520 12-bit (2×2 binned) | 45.2 | **default: 4:3 full FOV** (used at ≤ 40 fps) |
  | 4056×3040 12-bit | 11.7 | full-res stills (not used at this stage) |

  Measured values (M0 spike). The driver also offers 8- and 10-bit variants of
  each mode with higher frame rates.

- **Hardware H.264 encoder**: max 1920×1080 (level 4.2). The Pi 4 also has a
  hardware MJPEG encoder. Still JPEG encoding is **in software** (Picamera2
  0.3.26+ can encode YUV420 straight to JPEG).
- Runtime controls (`set_controls`) apply while running, a few frames later
  for exposure and gain. Changing the sensor mode, stream sizes or transform
  needs a reconfigure (stop, configure, start) **inside the process**.
- Camera buffers come from **CMA** (320 MB by default on Pi 4 models with more
  than 1 GB RAM). The default 2028×1520 + 1440×1080 YUV420 configuration
  uses ~7 MB per buffer set, which is comfortable.

---

## 2. High-level architecture

```
                                Raspberry Pi 4
┌────────────────────────────────────────────────────────────────────────────────┐
│                                                                                │
│  camhelper.py (Python, Picamera2)                 camweb (Go, stdlib only)     │
│  ┌──────────────────────────────────┐            ┌───────────────────────────┐ │
│  │ libcamera ── ISP ─┬─ main  (YUV) ─┼─ snapshot ─▶ files in captures/        │ │
│  │   IMX477          │                │             ▲  (helper writes, Go returns)│
│  │                   └─ lores (YUV) ─┤             │                          │ │
│  │                        │          │             │                          │ │
│  │                  H264Encoder (HW) │             │                          │ │
│  │                        │ whole AU │  video.sock │  ┌──────────────┐        │ │
│  │                  sender thread ───┼─────────────┼─▶│ helper.Client │        │ │
│  │                                   │  (binary)   │  │  video reader ├─▶ Hub ─┼─┐
│  │  control loop  ◀──────────────────┼─────────────┼──│  ctl JSON-RPC │        │ │ │
│  │  (configure, set_controls,        │   ctl.sock  │  └──────▲───────┘        │ │ │
│  │   snapshot)                ───────┼─────────────┼─▶ events (state, metadata) │ │
│  └──────────────────────────────────┘(JSON-RPC 2.0)│  ┌──────┴───────┐        │ │ │
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
                                REST: /api/config, /api/capture
```

### Division of responsibility (central for the later C++ rewrite)
- **The helper is thin.** It owns the camera and does only what needs
  libcamera: configure pipelines, apply controls, encode H.264, capture and
  save stills, report metadata. It holds **no policy** and stores no user
  config.
- **Go owns everything else**: the user-facing config model, validation,
  defaults, persistence, deciding how disruptive a change is, file naming,
  HTTP, fan-out to viewers, backpressure.
- They talk only through the protocol in [`PROTOCOL.md`](PROTOCOL.md). A C++
  helper that implements it is a drop-in replacement. A Go **conformance
  test** (planned before the C++ work) checks any helper against the protocol.

---

## 3. Camera pipeline (inside the helper)

### 3.1 Sensor mode and stream sizes
The prototype uses **one sensor mode: 2028×1520 12-bit (2×2 binned, full field
of view, up to 40 fps)**. Because of the rules in §1 (lores ≤ main with the same
aspect ratio, H.264 ≤ 1080p), the sizes are fixed except for the stream size:

| Stream | Size |
|---|---|
| sensor mode | 2028×1520 |
| main (snapshot source) | 2028×1520 (3.1 MP) |
| lores → H.264 (user choice) | **1440×1080** (default), 1024×768, 640×480 |
| fps | 1–40 |

The exact sizes (after `align_configuration`) are confirmed in the M0 spike and
then fixed as constants in Go (`internal/camera/config.go`). The helper takes
explicit sizes, so adding other sensor modes later doesn't touch the helper.

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
picam2.start_encoder(H264Encoder(bitrate=..., repeat=True, iperiod=gop, profile="high"),
                     SocketOutput(...), name="lores")
picam2.start()
```
`repeat=True` puts SPS/PPS before every IDR, so any viewer can start at any
keyframe. The H.264 profile is fixed at `high`. Flips come from startup flags
(`-hflip`, `-vflip`) and are not editable in the UI.

### 3.2 What each kind of change costs

| Change | Mechanism | Stream impact |
|---|---|---|
| Exposure (auto/manual, shutter, gain, EV, metering, mode), AWB (mode, manual gains), brightness, contrast, saturation, sharpness, noise reduction | `set_controls` (live) | **none**, takes effect after ~10 frames with 6 buffers (M0 0.7) |
| Bitrate, GOP, **frame rate** (also a manual shutter longer than the frame time) | `set_controls` if needed, then restart the encoder | new IDR, short gap. The frame rate needs an encoder restart: the Pi encoder assumes 30 fps, so the helper hands it `bitrate × 30 / fps` (M0 0.9) |
| Stream size | reconfigure inside the helper (stop → configure → start) | short gap, a few hundred ms expected (to measure) |
| Helper crash | Go restarts the helper | about 1–3 s (process and camera init) |

### 3.3 Still capture (snapshot from the live stream)
The helper takes the next completed request, copies the `main` buffer
(~4.6 MB for 2028×1520 YUV420), releases the request at once, and encodes
JPEG in a worker thread. The H.264 stream on lores keeps running, so there is
**no interruption**.
- Resolution = the main stream size, 2028×1520 (3.1 MP).
- Exposure and white balance are exactly those of the live stream (it is the
  same frame).
- The file is written to `<path>.jpg.tmp` and then renamed. Go sends it to the
  browser as a download and keeps the copy in `<data>/captures/` (there is no
  UI for managing them).

Not at this stage, but the design leaves room for it: a 12 MP capture by
switching mode inside the helper (short pause), and DNG from a raw stream.

### 3.4 Metadata
A Picamera2 `post_callback` samples per-frame metadata at about 2 Hz:
`ExposureTime`, `AnalogueGain`, `DigitalGain`, `ColourGains`,
`ColourTemperature`, `Lux`, `FrameDuration`, and `FocusFoM`
if present. These are sent as events. The UI shows them live, and offers a
**"lock current"** button that copies the current auto values into manual
settings. `FocusFoM` is shown as a raw number to help focus the HQ camera's
manual lens (§3.5).

### 3.5 Focus value (`FocusFoM`)
The UI's **Focus** number is libcamera's `FocusFoM` metadata, shown raw. It is
a **sharpness score computed by the image processor from the picture**, not
something the lens reports. The HQ camera has no autofocus, and the value is
computed for every camera.

How the Pi 4 computes it (Raspberry Pi libcamera `main` at the time of writing,
and kernel `rpi-6.6.y`):

1. The ISP runs a fixed focus (contrast) filter and accumulates its output in
   12 regions: *"Focus measure - accumulated output of the focus filter"*
   ([bcm2835-isp.h#L310-L324](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/include/uapi/linux/bcm2835-isp.h#L310-L324),
   [`FOCUS_REGIONS 12`](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/include/uapi/linux/bcm2835-isp.h#L278)).
2. On the Pi 4 the regions form a 4 × 3 grid
   ([controller.cpp#L40](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/controller.cpp#L40)).
3. libcamera uses the second filter's sum over pixels above the threshold,
   divided by 1000, per region
   ([vc4.cpp#L278-L282](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/vc4.cpp#L278-L282)).
4. `FocusFoM` is the average of the central regions only: on 4 × 3 that is the
   middle row, two middle columns, roughly the **middle third of the height and
   middle half of the width**
   ([ipa_base.cpp#L1641-L1663](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1641-L1663)).

libcamera's definition: *"A larger FocusFoM value indicates a more in-focus
frame … not necessarily aimed at providing a way to implement a focus algorithm
by the application, rather an indication of how in-focus a frame is"*
([control_ids_core.yaml#L657-L668](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/libcamera/control_ids_core.yaml#L657-L668)).

**Reading it:** higher is sharper. There is no unit and no absolute scale,
because the value is a sum that is not divided by the pixel count, so it depends
on scene detail, brightness and exposure. Compare values only for the same
scene and settings. Sensor noise at high gain raises it, so focus in good light
with exposure locked (*Lock current*).

**Not configurable:** nothing at runtime or in the tuning file changes how it
is computed.
- The ISP driver exposes no focus-statistics settings
  ([bcm2835-isp.h#L18-L35](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/include/uapi/linux/bcm2835-isp.h#L18-L35)).
- The grid, filter choice and centre averaging are fixed in code.
- `imx477.json` has no focus section.
- The autofocus window controls (`AfWindows`, `AfMetering`) exist only when a
  movable lens is present
  ([ipa_base.cpp#L105-L115](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L105-L115),
  [#L278-L280](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L278-L280)).

Possible later extensions:
- `StatsOutputEnable` delivers the raw per-region statistics
  ([control_ids_rpi.yaml#L10-L31](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/libcamera/control_ids_rpi.yaml#L10-L31)).
  From those the helper could build a 4 × 3 sharpness map or a different
  weighting.
- The helper could compute its own sharpness metric over a chosen region of
  the image.


### 3.6 Exposure control: EV, frame rate, shutter, gain

Raspberry Pi's libcamera auto-exposure algorithm (`rpi.agc`) is what the
helper drives. Links point to
[raspberrypi/libcamera @ 9fe1678](https://github.com/raspberrypi/libcamera/tree/9fe1678b8ad5fbae875942b55b58ade23014194c).

**Auto mode, every frame:**
1. `ExposureValue` (EV) becomes a multiplier 2^EV
   ([ipa_base.cpp#L1083-L1099](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1083-L1099)).
2. The brightness target from the tuning file (`y_target`, ≈ 0.16–0.17 of
   full scale for the HQ camera) is multiplied by 2^EV and **capped at 0.9**
   ([agc_channel.cpp#L693-L711](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L693-L711)). So EV above about
   **+2.4** has no further effect.
3. The needed total exposure (shutter × gain) is computed and **capped at
   max shutter × max gain** of the selected exposure profile. Max shutter is
   the profile's last step, limited by the frame time
   ([#L763-L786](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L763-L786)).
4. The frame time caps the shutter: `FrameDurationLimits` (the server sends
   min = max = 1/fps) determines the longest exposure the sensor allows, which
   becomes the algorithm's maximum
   ([ipa_base.cpp#L1780-L1810](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1780-L1810),
   [agc_channel.cpp#L972-L982](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L972-L982)).
5. The exposure is split by walking the profile's steps: shutter to the next
   step, then gain to the next step. Analogue gain is capped at the sensor
   maximum (22.26); digital gain makes up the rest, between 1 and
   `max_digital_gain` (default 4)
   ([#L881-L956](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L881-L956)).

HQ camera profiles (`imx477.json`):

| profile | shutter steps (µs) | gain steps |
|---|---|---|
| normal | 100, 10000, 30000, 60000, 66666 | 1, 2, 4, 6, 16 |
| short | 100, 5000, 10000, 20000, 66666 | 1, 2, 4, 6, 16 |
| long | 100, 10000, 30000, 60000, 120000 | 1, 2, 4, 6, 16 |

**Why EV can do nothing:** in a dark scene at 30 fps auto exposure sits at
33 ms × gain 16, the cap from steps 3–4 (M0 spike: `ExposureTime=33013
AnalogueGain=16.0`). EV raises a target that is already out of reach. To get a
brighter image in auto mode:
- lower the frame rate (15 fps allows the normal profile's 66.7 ms);
- use the "long" profile (120 ms) with ≤ 8 fps;
- or switch to manual.

**Manual mode:**
- `AeEnable` is a wrapper for `ExposureTimeMode`/`AnalogueGainMode`
  ([ipa_base.cpp#L1103-L1112](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1103-L1112)).
- Manual values are ignored while auto is on
  ([#L979-L997](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L979-L997),
  [#L1003-L1020](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1003-L1020)).
- **EV has no effect** in manual mode: the exposure is simply shutter × gain
  ([agc_channel.cpp#L765-L767](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L765-L767)). The UI disables EV,
  metering and profile when auto exposure is off.
- The shutter is still clamped by the frame time. The server stretches the
  frame time when the manual shutter is longer (`FrameDurationUS` in
  `internal/camera/controls.go`), which lowers the frame rate and restarts the
  encoder.
- libcamera accepts manual gain up to analogue max × max digital gain
  (≈ 89) ([#L985-L1000](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L985-L1000)). The UI currently stops at 22.

**Reading the actual values.** Per-frame metadata: `ExposureTime`,
`AnalogueGain`, `DigitalGain`, `FrameDuration`, and `AeState` (Idle / Searching /
Converged) ([ipa_base.cpp#L1590-L1619](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1590-L1619)).
- There is **no "limit reached" flag**.
- `Converged` only means the values stopped changing. libcamera says *"we
  don't know the exposure/gain limits of the sensor, so the values we keep
  requesting may be unachievable"*
  ([agc_channel.cpp#L527-L564](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L527-L564)). So a stuck dark
  scene reports Converged.

**What the UI does** (`internal/web/static/app.js`):
- The *Actual values* block shows shutter, analogue and digital gain, frame
  time, AE state, lux and colour temperature.
- In auto mode it shows a warning when **shutter ≥ 95 % of min(frame time,
  profile maximum) and analogue × digital gain ≥ 95 % of the profile's maximum
  gain**. The warning says which limit applies (frame time → lower the frame
  rate; profile → "long" profile or a lower frame rate) and that EV has no
  effect.
- Info popups (ⓘ) explain exposure overall, frame rate, shutter, gain, EV,
  metering, profiles and the actual values. The line under the profile select
  describes the selected profile and whether the frame time caps it.

**Where the profile data comes from.** libcamera's camera API does not expose
the profiles. The helper reads the tuning file the IPA loads for the sensor
(`<model>.json`, for example `/usr/share/libcamera/ipa/rpi/vc4/imx477.json`)
with Picamera2's `load_tuning_file()` / `find_tuning_algo()`
([picamera2.py#L264-L310](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/picamera2.py#L264-L310)) and sends the `rpi.agc`
exposure profiles, metering mode names, `y_target` and `max_digital_gain` in
`hello.tuning`. The server forwards them to the browser in a sticky `camera`
event. If the file cannot be read, the profile details and the limit warning
are not shown.

Caveat: if libcamera is pointed at a different tuning file
(`LIBCAMERA_RPI_TUNING_FILE`), the helper still reads `<model>.json`. Picamera2
clears that variable by default when it opens the camera
([picamera2.py#L336-L353](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/picamera2.py#L336-L353)).

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
- `ctl.sock`: **JSON-RPC 2.0**, one message per line (requests, responses,
  and notifications for events). Human-readable and
  scriptable.
- `video.sock`: length-prefixed binary frames. A large still never delays a
  control reply, because stills don't go over IPC at all.

**Why not an existing protocol on these sockets?**
- Control: **JSON-RPC 2.0** is used as-is (newline-delimited). gRPC (protobuf
  plus grpcio on the Pi) and HTTP over the Unix socket (pushing events needs
  SSE) cost more for no gain.
- Video: a raw Annex-B byte stream (Picamera2 `FileOutput`) is standard, but
  Go would have to find frame boundaries by waiting for the next start code
  (**+1 frame of latency**), and timestamps would be lost. `SOCK_SEQPACKET`
  hits message-size limits with large I-frames. RTP or MPEG-TS need muxing in
  Python and demuxing in Go. A 32-byte length-prefixed header is the least
  code and adds no latency.

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
  30 s) when it exits or either socket closes. There is no ping/heartbeat:
  liveness = the process is running and both sockets are open.
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
- **Existing alternatives reviewed and rejected for the prototype (v0.5):**
  WebSocket (standard framing, but there's no server in Go's standard library:
  it needs `x/net/websocket` or hand-written code); WebRTC with WHEP signalling
  via `pion` (best on lossy links, but heavy dependencies and NAT/ICE setup);
  the mediamtx media server (WebRTC/LL-HLS/RTSP; an extra binary, and its own
  camera source has no still capture). **Decision: `fetch` + a 16-byte
  header, zero dependencies.** WebRTC remains the upgrade path.
- WebCodecs requires a **secure context** (HTTPS or `localhost`). At this
  stage the app serves **plain HTTP only**. Ways to get a secure context
  without TLS in the app:
  - **SSH port forward** (LAN or internet):
    `ssh -L 8080:localhost:8080 pi@raspberrypi`, then open
    `http://localhost:8080`;
  - **Chrome/Edge flag** for LAN testing:
    `chrome://flags/#unsafely-treat-insecure-origin-as-secure`, then add
    `http://raspberrypi.local:8080`;
  - **an HTTPS tunnel or proxy** for internet use (TLS terminated outside the
    app): Tailscale Serve/Funnel, Cloudflare Tunnel, Caddy.

  Opening `http://<pi-ip>:8080` without one of these shows a page message
  explaining it.

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
- Server-side dropping is the **only** latency control; the browser does not
  skip frames itself.

### 5.4 Browser player (`player.js`, vanilla)
- `fetch('/stream')`, then `getReader()`, then parse headers. On an SPS change,
  `VideoDecoder.configure({codec: 'avc1.PPCCLL' from SPS, optimizeForLatency:
  true})`. No `description` is passed, so chunks are Annex-B.
  `hardwareAcceleration` is left at its default: Chrome treats
  `'prefer-hardware'` as hardware-only and fails on machines without a
  hardware H.264 decoder (found during implementation); the default still uses
  hardware when available.
- Only the newest decoded frame is drawn, on `requestAnimationFrame`; older
  frames are closed.
- No latency estimate in the browser: latency is measured glass-to-glass by
  hand (camera pointed at an on-screen ms clock). The `ts` field stays in the
  wire format for debugging.
- If the stream ends or fails, it reconnects every **2 s** (fixed interval).
  It keeps running in hidden tabs.

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
    StreamSize   Size   // 1440x1080 | 1024x768 | 640x480
    BufferCount  int
}
// Flips (-hflip/-vflip) are startup flags, not part of the editable config.
type Encoder struct {
    Bitrate int // bps
    GOP     int // frames between IDRs
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
}
type Still struct {
    Quality int // JPEG quality 1..100
}
```
- Ranges and defaults come from the helper's `hello` (libcamera
  `camera_controls` gives min/max/default for each control), plus Go-side
  sanity limits. Note (M0): `camera_controls` reflects the last configured
  sensor mode, so mode-dependent ranges such as `FrameDurationLimits` in
  `hello` are not used; only the ExposureTime/AnalogueGain maxima are.
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
| POST | `/api/config` | **the only settings endpoint**: partial JSON merged onto the config. Go decides what to do (live controls / encoder restart / reconfigure). Reply: `{"applied": "live"\|"encoder"\|"reconfigure", "config": {...}}` |
| POST | `/api/config/reset` | defaults |
| POST | `/api/capture` | optional `{"quality": 95}` → replies with **the JPEG itself** (`image/jpeg`, `Content-Disposition: attachment; filename=IMG_YYYYMMDD_HHMMSS.jpg`, headers `X-Capture-Width/Height`) |

Security: **none in the app at this stage** (no TLS, no auth). Run it on a
trusted network, or reach it through SSH forwarding, Tailscale or a tunnel
with its own access control. Validation stays strict: the helper gets only
validated, typed values, and file names are always generated by Go, never
taken from the client.

### 6.3 UI
- Live video canvas with stats overlay (fps, kbps, resolution, drops) and a
  state overlay (reconfiguring / reconnecting).
- **Settings form** with hard-coded fields and ranges (rendered by the
  template from Go constants; no capabilities endpoint). Every input sends
  `POST /api/config` on its **`change` event** (slider released, select
  changed, field committed), with no debounce and no Apply button. Fields that
  interrupt the stream briefly (stream size, bitrate, GOP) are labelled.
- **Actual values** from metadata (shutter, analogue/digital gain, frame time,
  AE state, lux, colour temperature), with a warning when auto exposure is at
  its limits (§3.6), and **Lock current**.
- The raw **Focus** value (`FocusFoM`, §3.5).
- ⓘ info popups for focus, exposure, frame rate, shutter, gain, EV, metering,
  profiles and actual values. EV, metering and profile are disabled in manual
  mode.
- **Capture** button: a snapshot without interrupting the stream; the
  browser downloads the JPEG.
- Collapsible `<details>` sections. Responsive layout for phones.

---

## 7. Repository layout

```
camera_rpi/
├── go.mod                          module github.com/mhbvr/camera_rpi (no requires)
├── cmd/
│   ├── camweb/main.go              server entry point
│   └── fakecam/main.go             fake helper (speaks PROTOCOL.md, plays a .h264 file) for dev/CI
├── camhelper/
│   ├── camhelper.py                Picamera2 helper (embedded into the Go binary)
│   ├── embed.go                    //go:embed camhelper.py
│   ├── test_camhelper.py           Python unit tests (no camera needed)
│   └── fake/                       fake picamera2 + libcamera to run camhelper.py without a Pi
├── internal/
│   ├── proto/                      PROTOCOL.md types: framing, messages (shared by server, fakecam, tests)
│   ├── helper/                     listener, session (ctl RPC + video reader), supervisor (spawn/restart)
│   ├── camera/                     Config, validation, controls mapping, diff, Manager, persistence
│   ├── stream/                     Hub, per-viewer queues, browser wire format
│   ├── h264/                       NAL type scan, SPS → codec string (small)
│   └── web/                        server, handlers, templates/, static/
├── spike/m0_spike.py               M0 hardware spike (run on the Pi)
├── deploy/camweb.service
└── docs/{DESIGN.md, PROTOCOL.md, PLAN.md}
```

---

## 8. Latency budget (1080p30, expected; measured in M1)

| Stage | Estimate |
|---|---|
| Sensor exposure, readout, ISP (sensor timestamp → request complete) | **40 ms measured** (33 ms exposure, dark scene; less with shorter exposures) |
| HW H.264 encode | **~18 ms p50 measured** (sensor → encoder output 53 ms p50, 60 ms mean, 73 ms p95) |
| Helper hand-off (Python callback, socket write) | socket 0.4 ms measured; snapshot JPEG encoding adds up to ~11 ms jitter (p95) |
| Go hub and HTTP write | ~1 ms |
| Network | LAN 1–5 ms; internet RTT/2 plus I-frame serialisation |
| Browser HW decode | 5–15 ms |
| Display (next vsync) | 0–16 ms |
| **Total** | **LAN ≈ 60–100 ms**; internet ≈ 120–300 ms depending on link |

---

## 9. Risks

| Risk | Mitigation |
|---|---|
| Python GIL: a JPEG encode of a 3 MP frame stalls the encoder output thread (which writes the video socket directly) and adds jitter | **measured (M0 0.5):** encode 95 ms, callback interval p95 35 → 46 ms, no dropped frames. Accepted; encoding stays in a thread |
| No helper-side queue: if Go stops reading `video.sock`, the encoder output thread blocks and the camera pipeline stalls | Go reads `video.sock` in a dedicated goroutine that only hands messages to the non-blocking hub; 1 MiB `SO_SNDBUF` absorbs short hiccups. Add a queue back if M1 shows stalls |
| Picamera2 API differences between versions (`Output.outputframe` signature, encoder args) | pin a minimum version in `hello`; use `*args, **kwargs` in the output adapter |
| TCP latency build-up over the internet | drop-to-keyframe, small send buffer; WebRTC as the upgrade path |
| WebCodecs needs a secure context, and the app has no TLS | SSH forward, browser flag, or an external HTTPS tunnel (§5.1); clear message in the page |
