# Helper API: camweb ↔ camera helper (protocol v1)

This is the contract between the Go server (`camweb`) and the camera helper
(`camhelper/camhelper.py` now, possibly a C++ binary later). Any program that
implements it can replace the helper, for example to support other cameras or
platforms ([HW_SPECIFIC.md](HW_SPECIFIC.md)).

- Why two Unix sockets and how the helper is started and supervised:
  [ARCHITECTURE.md §3](ARCHITECTURE.md#3-processes-and-ipc).
- Implementations in this repository: `camhelper/camhelper.py` (Picamera2),
  `cmd/fakecam` (Go fake that replays a recorded H.264 file, for development
  without a Pi), and the Go side in `internal/proto` and `internal/helper`.
- Try it by hand: start `camweb -helper external`, then run the helper
  yourself, or connect to `ctl.sock` with `socat - UNIX-CONNECT:ctl.sock`
  while no helper is connected and type the JSON messages below.

The browser-facing HTTP API is a different document:
[REST_API.md](REST_API.md).

## 1. Transport

- Two **Unix domain sockets, SOCK_STREAM**, created by the **server**
  (listener, mode 0600):
  - `ctl.sock`: control, **JSON-RPC 2.0** messages, UTF-8, one JSON object
    per line (`\n` terminated, max 1 MiB per line), §3.
  - `video.sock`: binary video messages (§4).
- The helper is started as
  `camhelper --ctl <path> --video <path> [--log-level info]`
  and **connects to `ctl.sock` first, then `video.sock`**, retrying for up to
  10 s.
- One helper session = one ctl connection plus one video connection. If either
  closes, the session ends. The server closes the other one; the helper should
  release the camera and exit (in spawn mode the server starts a new one).
- The helper writes **logs to stderr only**. Stdout is unused.
- Byte order of binary fields: **big-endian**.

## 2. Session lifecycle

```
helper                                   server
  │── connect ctl.sock ──────────────────▶│
  │── connect video.sock ────────────────▶│
  │── notification hello ────────────────▶│  check proto version
  │◀──────── request id=1 configure ──────│
  │── result id=1 ───────────────────────▶│
  │== video: stream_start, AU, AU, … ═══▶│
  │◀──────── request id=2 set_controls ───│
  │── result id=2 ───────────────────────▶│
  │── notification metadata (2 Hz) ──────▶│
  │◀──────── request id=3 shutdown ───────│
  │── result id=3, then exit ────────────▶│
```
Until the first successful `configure`, the helper is **idle**: the camera is
opened, not started, and no video is sent.

## 3. Control channel (`ctl.sock`)

### 3.1 Message format: JSON-RPC 2.0
The control channel uses **[JSON-RPC 2.0](https://www.jsonrpc.org/specification)**
unchanged, framed as **one JSON object per line** (newline-delimited; JSON
encoders on both sides emit single-line JSON by default).

```jsonc
// server → helper: request
{"jsonrpc": "2.0", "id": 7, "method": "set_controls", "params": { ... }}
// helper → server: success response (exactly one per request, same id)
{"jsonrpc": "2.0", "id": 7, "result": { ... }}
// helper → server: error response
{"jsonrpc": "2.0", "id": 7, "error": {"code": -32602, "message": "ExposureTime out of range", "data": {"field": "ExposureTime"}}}
// helper → server: notification (no id, never answered), any time
{"jsonrpc": "2.0", "method": "metadata", "params": { ... }}
```
- **Roles:** the server sends requests; the helper sends responses and
  **notifications** (the "events" in this document). The server never sends
  notifications in v1, and the helper never sends requests.
- `id`: a positive integer chosen by the server. Responses can come **out of
  order** (a snapshot's JPEG encode must not block `set_controls`), so the
  server matches them by id.
- `params` is always a JSON **object** (named parameters). It is omitted for
  methods without parameters. A method that returns nothing returns
  `"result": {}`.
- **Batches** (JSON arrays) are not used. The helper answers an array with
  `-32600 Invalid Request`.
- Methods that touch the camera configuration (`configure`, `snapshot`,
  `set_encoder`, `stop`) run **one at a time** inside the helper. If another
  is already running, the helper replies with the `busy` error (the server
  serialises them anyway).
- Implementation: there is no library on either side. The Go side is about
  100 lines in `internal/proto` (Go's `net/rpc/jsonrpc` speaks JSON-RPC 1.0,
  so it isn't used). The Python side is a small dispatch table.

**Error codes** (standard codes plus our range from the spec's
`-32000…-32099` implementation-defined block):

| code | name | meaning |
|---|---|---|
| -32700 | Parse error | invalid JSON |
| -32600 | Invalid Request | not a valid request object (or a batch) |
| -32601 | Method not found | unknown or unsupported method |
| -32602 | Invalid params | bad or out-of-range params; `data.field` names the field |
| -32603 | Internal error | unexpected exception in the helper |
| -32001 | busy | another camera operation is running |
| -32002 | not_configured | needs a successful `configure` first |
| -32003 | camera_error | libcamera/Picamera2 failure |
| -32004 | io_error | file or socket I/O failure (for example writing the snapshot) |

### 3.2 Notifications (helper → server)
| method | params | when |
|---|---|---|
| `hello` | see §3.4 | first message on connect |
| `state` | `{"state": "idle"\|"starting"\|"streaming"\|"reconfiguring"\|"error", "detail": "..."}` | on every change |
| `metadata` | `{"ExposureTime": 8000, "AnalogueGain": 2.1, "DigitalGain": 1.0, "ColourGains": [1.8, 1.5], "ColourTemperature": 4800, "Lux": 320.5, "FrameDuration": 33333, "FocusFoM": 1234, "AeState": 2, "SensorTimestamp": 123456789000}` (keys present when available) | ~2 Hz while streaming (`metadata_hz` in configure) |
| `stats` | `{"fps": 30.0, "frames": 12345, "latency_ms": 45.2}` (`latency_ms`: mean sensor timestamp → socket write over recent frames, when known) | 1 Hz |
| `error` | `{"code": -32003, "message": "..."}` | asynchronous errors (for example the encoder failed) |

### 3.3 Methods

#### `configure`
Configures the pipeline and starts streaming. If it is already running, the
helper stops, reconfigures and restarts (the video channel gets a new
`stream_start`).
```json
{"jsonrpc":"2.0","id":1,"method":"configure","params":{
  "sensor":  {"size": [2028,1520], "bit_depth": 12},
  "main":    {"size": [2028,1520], "format": "YUV420"},
  "lores":   {"size": [1440,1080], "format": "YUV420"},
  "transform": {"hflip": false, "vflip": false},     // from server startup flags
  "buffer_count": 6,
  "controls": {"FrameDurationLimits": [33333,33333], "AeEnable": true},
  "encoder": {"codec": "h264", "bitrate": 6000000, "iperiod": 30, "framerate": 30.0},   // H.264 profile fixed: high
  "metadata_hz": 2
}}
→ {"jsonrpc":"2.0","id":1,"result":{
     "applied": { ...same shape, with the actual sizes/strides after alignment... },
     "session": 3
}}
```

#### `set_controls`
Applies libcamera controls live. Names and values are libcamera's (the server
does the mapping). Unknown control → `-32602 Invalid params`.
```json
{"jsonrpc":"2.0","id":2,"method":"set_controls","params":{"controls":{
  "AeEnable": false, "ExposureTime": 10000, "AnalogueGain": 2.0,
  "AwbEnable": true, "AwbMode": 0, "ColourGains": [1.8,1.5],
  "ExposureValue": 0.0, "AeMeteringMode": 0, "AeExposureMode": 0,
  "Brightness": 0.0, "Contrast": 1.0, "Saturation": 1.0, "Sharpness": 1.0,
  "NoiseReductionMode": 1,
  "FrameDurationLimits": [20000,20000]
}}}
→ {"jsonrpc":"2.0","id":2,"result":{}}
```
Enum controls are sent as **integers** (libcamera enum values), so the helper
needs no name tables. The names come from `hello.controls[*].values`.

#### `set_encoder`
Restarts only the encoder with new parameters. The video channel gets a new
`stream_start` and starts with an IDR.
```json
{"jsonrpc":"2.0","id":3,"method":"set_encoder","params":{"bitrate":4000000,"iperiod":30,"framerate":30.0}}
→ {"jsonrpc":"2.0","id":3,"result":{"session": 4}}
```

`framerate` (in `configure.encoder` and `set_encoder`) is the actual frame
rate, and `bitrate` is the bitrate to deliver. The helper must give the
encoder both: the Pi's encoders plan bits per frame for 30 fps unless they get
the frame rate (`VIDIOC_S_PARM`), and then deliver requested × fps / 30
([HARDWARE.md §3.1](HARDWARE.md#31-why-the-encoders-assume-30-fps)). The Picamera2 helper passes
`framerate=…, enable_sps_framerate=True` with the requested bitrate. The
server sends `set_encoder` after any frame-rate change, because the frame rate
is set when the encoder starts.

#### `snapshot`
Captures one frame from the running `main` stream as JPEG **without
interrupting video**.
```json
{"jsonrpc":"2.0","id":4,"method":"snapshot","params":{
  "path": "/dev/shm/camweb-captures/IMG_20261106_134501",   // no extension; helper appends .jpg
  "quality": 95
}}
→ {"jsonrpc":"2.0","id":4,"result":{
  "file": {"path": ".../IMG_20261106_134501.jpg", "size": 1834221, "width": 2028, "height": 1520},
  "metadata": { ...frame metadata... },
  "timing_ms": {"capture": 35, "encode": 180}
}}
```
Rules:
- The helper writes `<path>.jpg.tmp`, then renames it to `<path>.jpg`.
- The response is sent **after** the file is complete.
- The `path` comes from the server only (never from a browser client).
- If not streaming → `-32002 not_configured`.

#### `set_mjpeg`
Starts or stops the MJPEG fallback encoder (a second, hardware encoder on the
same `lores` stream, [ARCHITECTURE.md §5.3](ARCHITECTURE.md#53-mjpeg-fallback)). It keeps running across `configure`
(restarted with the pipeline) until disabled. Frames arrive on `video.sock` as
type 4.
```json
{"jsonrpc":"2.0","id":6,"method":"set_mjpeg","params":{"enabled":true,"bitrate":8000000,"skip":2,"framerate":30.0}}
→ {"jsonrpc":"2.0","id":6,"result":{"active":true}}
```
`skip` = encode every skip-th camera frame (Picamera2 `frame_skip_count`).
`framerate` = the camera frame rate (as in `set_encoder`), so the MJPEG
encoder runs at framerate / skip. `bitrate` is the bitrate to deliver. The
Picamera2 helper gives the MJPEG encoder its frame rate through the
attributes `MJPEGEncoder` inherits from `V4L2Encoder`
([HARDWARE.md §3.1](HARDWARE.md#31-why-the-encoders-assume-30-fps)). The
server re-sends `set_mjpeg` after a frame-rate change.
`active` is false when the camera is not streaming (the encoder then starts
with the next `configure`).

#### `stop`
Stops streaming and returns to idle (the camera stays open). No params; result `{}`.

#### `shutdown`
No params. Respond with `{}`, release the camera, exit 0.

### 3.4 `hello`
```json
{"jsonrpc":"2.0","method":"hello","params":{
  "proto": 1,
  "impl": "picamera2", "impl_version": "0.3.27", "libcamera": "v0.5.x",
  "camera": {
    "model": "imx477",
    "pixel_array": [4056,3040],
    "scaler_crop_max": [0,0,4056,3040],
    "sensor_modes": [
      {"size":[1332,990],"bit_depth":10,"fps":120.05,"crop_limits":[696,528,2664,1980],"exposure_limits":[31,667234896]},
      {"size":[2028,1080],"bit_depth":12,"fps":50.03,"crop_limits":[0,440,4056,2160]},
      {"size":[2028,1520],"bit_depth":12,"fps":40.01,"crop_limits":[0,0,4056,3040]},
      {"size":[4056,3040],"bit_depth":12,"fps":10.0,"crop_limits":[0,0,4056,3040]}
    ],
    "controls": {
      "ExposureTime": {"min": 31, "max": 667234896, "default": 20000},
      "AnalogueGain": {"min": 1.0, "max": 22.26, "default": 1.0},
      "AwbMode": {"min": 0, "max": 7, "default": 0, "values": {"Auto":0,"Incandescent":1,"Tungsten":2,"Fluorescent":3,"Indoor":4,"Daylight":5,"Cloudy":6,"Custom":7}},
      "...": {}
    }
  },
  "features": ["snapshot", "metadata", "set_encoder", "mjpeg"]
}}
```
The values above are illustrative. The helper reports what libcamera says.
`features` lets a partial implementation (for example an early C++ helper)
declare what it supports.

## 4. Video channel (`video.sock`)

Helper → server only. A sequence of messages, each a **32-byte header** plus
payload:

```
offset size field
0      4    payload length (u32)
4      1    type          1 = H.264 access unit, 2 = stream_start (JSON), 3 = stream_end (JSON), 4 = JPEG frame
5      1    flags         bit0 keyframe (IDR), bit1 contains SPS/PPS
6      2    reserved (0)
8      8    seq           u64 frame counter, reset to 0 at each stream_start
16     8    pts_us        u64 sensor timestamp of the frame in µs, CLOCK_MONOTONIC (Picamera2's relative timestamp + encoder.firsttimestamp; relative to the first frame if that is unavailable)
24     8    wall_us       u64 helper wall clock (µs since Unix epoch) when the AU was handed to the socket
32     N    payload
```

- **type 1 (AU):** exactly **one complete access unit** (one frame) in
  **Annex-B** format (start codes included). With `repeat=True`, IDR AUs carry
  SPS+PPS (flags bit1). The first AU after `stream_start` **must be an IDR**.
- **type 2 (stream_start):** JSON payload
  `{"session": 3, "codec": "h264", "width": 1440, "height": 1080, "fps": 30.0, "profile": "high", "bitrate": 6000000, "iperiod": 30}`.
  `fps` is the frame rate the encoder was started with (`framerate` from
  `configure` / `set_encoder`).
  Sent whenever the encoder (re)starts. The server resets all viewers to
  "wait for keyframe".
- **type 3 (stream_end):** JSON `{"session": 3, "reason": "reconfigure"|"encoder"|"stop"|"error"}`.
- **type 4 (JPEG frame):** one complete JPEG from the MJPEG fallback encoder
  (only while enabled with `set_mjpeg`). `seq` counts JPEG frames; `pts_us` is
  the encoder timestamp.

### Flow control
- **No helper-side queue**: the encoder output callback writes each AU
  straight to `video.sock` (`sendall`, under a lock shared with nothing else).
  A slow reader would therefore block the encoder thread, so:
- the server **must** read `video.sock` continuously, in a dedicated goroutine
  that only hands messages to the non-blocking hub (which applies its own
  per-viewer drop policy);
- the helper sets `SO_SNDBUF` to 1 MiB (one I-frame or more) to absorb short
  hiccups.
- Liveness: there is no ping. A session is alive while the helper process runs
  and both sockets are open.

## 5. Versioning
- `hello.proto` is an integer. The server refuses (logs and restarts) a helper
  whose major `proto` differs from its own.
- New optional methods, params and notification fields can be added without
  a version bump. Receivers **must ignore unknown fields**.

## 6. Writing another helper

- Implement at least `hello`, `configure`, `set_controls`, `set_encoder`,
  `stop`, `shutdown` and the video types 1–3. List optional parts in
  `hello.features` (`snapshot`, `metadata`, `mjpeg`).
- `set_controls` receives **libcamera control names and values**: the server
  maps its settings to libcamera in `internal/camera/controls.go`. A helper
  for a non-libcamera camera has to translate them.
- The sizes in `configure` come from constants in `internal/camera/config.go`
  (sensor mode, main, stream sizes). They describe the Pi 4 + HQ camera and
  must be changed for other hardware ([HW_SPECIFIC.md](HW_SPECIFIC.md)).
- Test with `camweb -helper external` (or `-helper-cmd "your-helper"`; the
  server appends `--ctl` and `--video`) and the bundled web page.

## 7. Future extensions (reserved, not in v1)
- **`capture_full`**: a full sensor resolution (4056×3040) capture by
  switching mode inside the helper (stop encoder → `switch_mode` → capture →
  switch back → start encoder). Video pauses briefly and a new `stream_start`
  follows.
- **Raw/DNG**: `configure.raw` plus `snapshot.raw` to also save a DNG.
- **Raw frame sharing (shared memory):** command `share_frames {"stream":"lores","slots":4}`.
  The helper replies with a `memfd` passed via `SCM_RIGHTS` on `ctl.sock`, and
  sends `frame_ready {"slot":n,"seq":…}` notifications. The server answers with
  `frame_release {"slot":n}` (this would be the first server → helper
  notification). For processing (OpenCV and similar) without
  copying.
- `capture_burst`, `start_recording` (to a file on the Pi).
