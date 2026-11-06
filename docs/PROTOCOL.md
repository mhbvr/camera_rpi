# Camera helper IPC protocol (v1)

This is the contract between the Go server (`camweb`) and the camera helper
(`camhelper.py` now, possibly a C++ binary later). Any helper that implements
this document is a drop-in replacement. A conformance suite
(`internal/conformance`, planned before the C++ helper) will test a helper
against it.

## 1. Transport

- Two **Unix domain sockets, SOCK_STREAM**, created by the **server**
  (listener, mode 0600):
  - `ctl.sock`: control, UTF-8 **JSON lines** (one JSON object per line, `\n`
    terminated, max 1 MiB per line).
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
  │── {"event":"hello", ...} ────────────▶│  check proto version
  │◀────────── {"id":1,"cmd":"configure",...}
  │── {"id":1,"ok":true,...} ────────────▶│
  │== video: stream_start, AU, AU, … ═══▶│
  │◀────────── {"id":2,"cmd":"set_controls",...}
  │── {"id":2,"ok":true} ────────────────▶│
  │── {"event":"metadata",...} (2 Hz) ───▶│
  │◀────────── {"id":3,"cmd":"shutdown"}
  │── {"id":3,"ok":true} and exit ───────▶│
```
Until the first successful `configure`, the helper is **idle**: the camera is
opened, not started, and no video is sent.

## 3. Control channel (`ctl.sock`)

### 3.1 Message shapes
```json
// server → helper: request
{"id": 7, "cmd": "set_controls", "args": { ... }}
// helper → server: response (exactly one per request, same id)
{"id": 7, "ok": true, "result": { ... }}
{"id": 7, "ok": false, "error": {"code": "invalid_args", "message": "..."}}
// helper → server: event (no id), any time
{"event": "metadata", "data": { ... }}
```
- `id`: positive integer chosen by the server. Responses can come **out of
  order** (a snapshot's JPEG encode must not block `ping`), so the server matches
  them by id.
- Requests that touch the camera configuration (`configure`, `snapshot`,
  `set_encoder`, `stop`) run **one at a time** inside the
  helper. If another is already running, the helper either queues it or
  replies `busy`. v1: **reply `busy`**; the server serialises anyway.
- Error codes: `invalid_args`, `busy`, `not_configured`, `camera_error`,
  `io_error`, `unsupported`, `internal`.

### 3.2 Events
| event | data | when |
|---|---|---|
| `hello` | see §3.4 | first message on connect |
| `state` | `{"state": "idle"\|"starting"\|"streaming"\|"reconfiguring"\|"error", "detail": "..."}` | on every change |
| `metadata` | `{"ExposureTime": 8000, "AnalogueGain": 2.1, "DigitalGain": 1.0, "ColourGains": [1.8, 1.5], "ColourTemperature": 4800, "Lux": 320.5, "FrameDuration": 33333, "ScalerCrop": [x,y,w,h], "FocusFoM": 1234, "SensorTimestamp": 123456789000}` (keys present when available) | ~2 Hz while streaming (`metadata_hz` in configure) |
| `stats` | `{"fps": 30.0, "frames": 12345, "dropped": 0, "queue": 1}` | 1 Hz |
| `error` | `{"code": "...", "message": "..."}` | asynchronous errors (for example the encoder failed) |

### 3.3 Commands

#### `configure`
Configures the pipeline and starts streaming. If it is already running, the
helper stops, reconfigures and restarts (the video channel gets a new
`stream_start`).
```json
{"id":1,"cmd":"configure","args":{
  "sensor":  {"size": [2028,1520], "bit_depth": 12},
  "main":    {"size": [2028,1520], "format": "YUV420"},
  "lores":   {"size": [1440,1080], "format": "YUV420"},
  "transform": {"hflip": false, "vflip": false},
  "buffer_count": 6,
  "controls": {"FrameDurationLimits": [33333,33333], "AeEnable": true},
  "encoder": {"codec": "h264", "bitrate": 6000000, "iperiod": 30, "profile": "high"},
  "metadata_hz": 2
}}
→ {"id":1,"ok":true,"result":{
     "applied": { ...same shape, with the actual sizes/strides after alignment... },
     "session": 3
}}
```

#### `set_controls`
Applies libcamera controls live. Names and values are libcamera's (the server
does the mapping). Unknown control → `invalid_args`.
```json
{"id":2,"cmd":"set_controls","args":{"controls":{
  "AeEnable": false, "ExposureTime": 10000, "AnalogueGain": 2.0,
  "AwbEnable": true, "AwbMode": 0, "ColourGains": [1.8,1.5],
  "ExposureValue": 0.0, "AeMeteringMode": 0, "AeExposureMode": 0,
  "Brightness": 0.0, "Contrast": 1.0, "Saturation": 1.0, "Sharpness": 1.0,
  "NoiseReductionMode": 1, "ScalerCrop": [0,0,4056,3040],
  "FrameDurationLimits": [20000,20000]
}}}
→ {"id":2,"ok":true}
```
Enum controls are sent as **integers** (libcamera enum values), so the helper
needs no name tables. The names come from `hello.controls[*].values`.

#### `set_encoder`
Restarts only the encoder with new parameters. The video channel gets a new
`stream_start` and starts with an IDR.
```json
{"id":3,"cmd":"set_encoder","args":{"bitrate":4000000,"iperiod":30,"profile":"high"}}
```

#### `snapshot`
Captures one frame from the running `main` stream as JPEG **without
interrupting video**.
```json
{"id":4,"cmd":"snapshot","args":{
  "path": "/home/pi/data/captures/IMG_20261106_134501",   // no extension; helper appends .jpg
  "quality": 95
}}
→ {"id":4,"ok":true,"result":{
  "file": {"path": ".../IMG_20261106_134501.jpg", "size": 1834221, "width": 2028, "height": 1520},
  "metadata": { ...frame metadata... },
  "timing_ms": {"capture": 35, "encode": 180}
}}
```
Rules:
- The helper writes `<path>.jpg.tmp`, then renames it to `<path>.jpg`.
- The response is sent **after** the file is complete.
- The `path` comes from the server only (never from a browser client).
- If not streaming → `not_configured`.

#### `stop`
Stops streaming and returns to idle (the camera stays open).

#### `ping`
`{"id":9,"cmd":"ping"}` → `{"id":9,"ok":true,"result":{"mono_ns": 123, "wall_us": 456}}`.
Used for a liveness check (every 5 s; 3 missed pings → the server restarts the
helper) and for measuring IPC latency.

#### `shutdown`
Respond, release the camera, exit 0.

### 3.4 `hello`
```json
{"event":"hello","data":{
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
  "features": ["snapshot", "metadata", "set_encoder"]
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
4      1    type          1 = H.264 access unit, 2 = stream_start (JSON), 3 = stream_end (JSON)
5      1    flags         bit0 keyframe (IDR), bit1 contains SPS/PPS
6      2    reserved (0)
8      8    seq           u64 frame counter, reset to 0 at each stream_start
16     8    pts_us        u64 sensor timestamp of the frame in µs (libcamera SensorTimestamp; clock domain checked in M0)
24     8    wall_us       u64 helper wall clock (µs since Unix epoch) when the AU was handed to the socket
32     N    payload
```

- **type 1 (AU):** exactly **one complete access unit** (one frame) in
  **Annex-B** format (start codes included). With `repeat=True`, IDR AUs carry
  SPS+PPS (flags bit1). The first AU after `stream_start` **must be an IDR**.
- **type 2 (stream_start):** JSON payload
  `{"session": 3, "codec": "h264", "width": 1440, "height": 1080, "fps": 30.0, "profile": "high", "bitrate": 6000000, "iperiod": 30}`.
  Sent whenever the encoder (re)starts. The server resets all viewers to
  "wait for keyframe".
- **type 3 (stream_end):** JSON `{"session": 3, "reason": "reconfigure"|"encoder"|"stop"|"error"}`.

### Flow control
- The helper keeps a **bounded queue** (default 60 AUs) between the encoder
  callback and the socket-writing thread, so a slow reader never blocks the
  camera or encoder threads. When the queue is full, it drops AUs **until the
  next IDR** and reports the count in `stats.dropped`.
- The server reads `video.sock` continuously, in a dedicated goroutine, into
  the hub (which applies its own per-viewer policy). In normal operation the
  helper-side queue stays at 0–1.
- Recommended `SO_SNDBUF` on the helper side: 1 MiB (one I-frame or more).

## 5. Versioning
- `hello.proto` is an integer. The server refuses (logs and restarts) a helper
  whose major `proto` differs from its own.
- New optional commands, args and event fields can be added without a version
  bump. Receivers **must ignore unknown fields**.

## 6. Future extensions (reserved, not in v1)
- **`capture_full`**: a full sensor resolution (4056×3040) capture by
  switching mode inside the helper (stop encoder → `switch_mode` → capture →
  switch back → start encoder). Video pauses briefly and a new `stream_start`
  follows.
- **Raw/DNG**: `configure.raw` plus `snapshot.raw` to also save a DNG.
- **Raw frame sharing (shared memory):** command `share_frames {"stream":"lores","slots":4}`.
  The helper replies with a `memfd` passed via `SCM_RIGHTS` on `ctl.sock`, and
  sends `frame_ready {"slot":n,"seq":…}` events. The server acknowledges with
  `frame_release {"slot":n}`. For processing (OpenCV and similar) without
  copying.
- `capture_burst`, `start_recording` (to a file on the Pi), MJPEG codec.
