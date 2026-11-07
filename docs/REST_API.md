# REST API

Everything the bundled web page does goes through the HTTP API below, so an
alternative interface (a native app, a script, another web UI, a recorder)
can do the same: read and change all settings, capture snapshots, watch the
live state and metadata, and receive the video as H.264 or MJPEG.

- Base URL: `http://<pi>:8080` (or `https://` with `-tls-cert`/`-tls-key`;
  see the [README](../README.md#opening-the-page)).
- JSON requests and responses use UTF-8 and `Content-Type: application/json`.
- **No authentication.** Run camweb on a trusted network or behind a tunnel
  or proxy with its own access control.
- A UI served from another origin needs `camweb -cors-origin
  https://my-ui.example` (or `*`); see [CORS](#cors).

## Endpoints

| Method | Path | Returns | Section |
|---|---|---|---|
| GET | `/api/config` | current settings (JSON) | [Config](#config) |
| POST | `/api/config` | change settings (partial JSON) | [Config](#config) |
| POST | `/api/config/reset` | restore defaults | [Config](#config) |
| GET | `/api/schema` | every setting with type, range, values, defaults | [Schema](#schema) |
| GET | `/api/status` | latest state, camera, stream, metadata, stats; client counts | [Status](#status) |
| GET | `/api/events` | live events as Server-Sent Events | [Events](#events) |
| POST | `/api/capture` | a 2028×1520 JPEG snapshot | [Capture](#capture) |
| GET | `/stream` | H.264 + events in a small binary framing (used by the page) | [/stream](#stream) |
| GET | `/stream.h264` | raw H.264 Annex-B elementary stream | [/stream.h264](#streamh264) |
| GET | `/stream.mjpeg` | MJPEG, `multipart/x-mixed-replace` | [/stream.mjpeg](#streammjpeg) |
| GET | `/`, `/static/…` | the bundled web page | — |

## Config

`GET /api/config` returns the whole configuration:

```json
{
  "pipeline": {"stream_size": [1440, 1080], "buffer_count": 6},
  "encoder":  {"bitrate": 6000000, "gop": 30},
  "controls": {
    "fps": 30, "auto_exposure": true, "shutter_us": 10000, "gain": 1, "ev": 0,
    "metering": "centre", "exposure_mode": "normal",
    "auto_wb": true, "awb_mode": "auto", "wb_gain_r": 1.5, "wb_gain_b": 1.5,
    "brightness": 0, "contrast": 1, "saturation": 1, "sharpness": 1, "denoise": "auto"
  },
  "still": {"quality": 95},
  "mjpeg": {"bitrate": 8000000, "skip": 2}
}
```

`POST /api/config` takes **any subset** of that document, merges it onto the
current configuration, validates the result and applies it:

```bash
curl -s -X POST http://raspberrypi.local:8080/api/config \
  -H 'Content-Type: application/json' \
  -d '{"controls": {"auto_exposure": false, "shutter_us": 20000, "gain": 4}}'
```

```json
{"applied": "live", "config": { "...": "the full new configuration" }}
```

- `applied` says what the change cost: `none` (nothing changed, or only
  `still`/`mjpeg`), `live` (camera controls, no interruption), `encoder`
  (encoder restart: new keyframe, short gap; bitrate, GOP, frame rate) or
  `reconfigure` (pipeline restart: stream size, buffer count). Details per
  setting: [HARDWARE.md §4](HARDWARE.md#4-settings-reference).
- Unknown fields are rejected. On error nothing is applied.
- The configuration is saved to `<data>/config.json` and re-applied whenever
  the camera helper (re)connects.
- Without a connected camera, changes are validated and saved (`applied`
  then says what they would cost) and applied when the camera comes up.

`POST /api/config/reset` restores the defaults; same response.

What every setting does, and the libcamera control behind it:
[HARDWARE.md](HARDWARE.md#4-settings-reference).

## Schema

`GET /api/schema` describes every setting, in the bundled UI's order, so a
client can build its form without hard-coding ranges:

```json
{
  "fields": [
    {"path": "pipeline.stream_size", "type": "size", "sizes": [[1440,1080],[1024,768],[640,480]], "unit": "px", "apply": "reconfigure", "note": "..."},
    {"path": "encoder.bitrate", "type": "int", "min": 250000, "max": 25000000, "unit": "bit/s", "apply": "encoder", "note": "delivered H.264 bitrate"},
    {"path": "controls.fps", "type": "float", "min": 1, "max": 40, "unit": "fps", "apply": "encoder", "note": "..."},
    {"path": "controls.auto_exposure", "type": "bool", "apply": "live"},
    {"path": "controls.metering", "type": "enum", "values": ["centre", "spot", "matrix"], "apply": "live"},
    "..."
  ],
  "defaults": { "...": "same shape as GET /api/config" },
  "sensor_size": [2028, 1520],
  "main_size": [2028, 1520]
}
```

- `path` is the setting's location in the config (`section.field`); a
  partial config for it is `{"section": {"field": value}}`.
- `type`: `int`, `float`, `bool`, `enum` (`values`), `size` (`sizes`, each
  `[width, height]`).
- `min`/`max` are exactly what the server accepts (a test keeps them in sync
  with validation). The camera can be stricter: gain and shutter above the
  camera's own maximum are rejected (`status.camera.sensor_max_gain`).
- `apply` is what a change does: `reconfigure`, `encoder`, `live`,
  `capture` (used per snapshot) or `mjpeg` (restarts only the MJPEG encoder,
  only while MJPEG viewers are connected).
- The bundled UI uses narrower slider ranges for contrast, saturation and
  sharpness (0–4, 0–4, 0–8).

## Status

`GET /api/status` returns the latest event of each kind (see
[Events](#events); a key is missing until its first event) and the number of
connected stream clients:

```json
{
  "state":    {"event": "state", "state": "streaming"},
  "camera":   {"event": "camera", "model": "imx477", "impl": "picamera2", "impl_version": "0.3.27", "sensor_max_gain": 22.26},
  "stream":   {"event": "stream", "session": 1, "codec": "h264", "width": 1440, "height": 1080, "fps": 30, "profile": "high", "bitrate": 6000000, "iperiod": 30},
  "metadata": {"event": "metadata", "data": {"ExposureTime": 33013, "AnalogueGain": 16.0, "Lux": 12.3, "FocusFoM": 1234, "...": "..."}},
  "stats":    {"event": "stats", "data": {"fps": 30, "frames": 12345, "latency_ms": 54.1}},
  "clients":  {"stream": 1, "mjpeg": 0}
}
```

`clients.stream` counts `/stream`, `/stream.h264` and `/api/events`
connections; `clients.mjpeg` counts `/stream.mjpeg`.

## Events

The server forwards these JSON events. Each has an `event` field.

| Event | Fields | When |
|---|---|---|
| `state` | `state`, optional `detail` | on every change. `waiting` (no helper yet), `starting`, `streaming`, `reconfiguring`, `idle`, `helper-restarting`, `error` |
| `camera` | `model`, `impl`, `impl_version`, `sensor_max_gain` | when a helper connects |
| `stream` | `session`, `codec`, `width`, `height`, `fps`, `profile`, `bitrate`, `iperiod` | when the H.264 encoder (re)starts; video continues from a keyframe |
| `metadata` | `data`: libcamera metadata (`ExposureTime` µs, `AnalogueGain`, `DigitalGain`, `ColourGains`, `ColourTemperature` K, `Lux`, `FrameDuration` µs, `FocusFoM`, `AeState`, `SensorTimestamp`) | about 2 Hz while streaming ([HARDWARE.md §6](HARDWARE.md#6-metadata-and-actual-values)) |
| `stats` | `data`: `fps`, `frames`, `latency_ms` (sensor → helper socket) | 1 Hz while streaming |
| `drop` | `dropped`, `total` | `/stream` only: frames this client lost because it fell behind |
| `error` | `data`: `code`, `message` | asynchronous camera errors |

**`GET /api/events`** delivers them as
[Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html):
one `data:` line with the JSON per event, no `event:` name, a `: keepalive`
comment every 15 s. The latest `state`, `camera`, `stream`, `metadata` and
`stats` are sent first.

```js
const es = new EventSource('http://raspberrypi.local:8080/api/events');
es.onmessage = (e) => {
  const ev = JSON.parse(e.data);
  if (ev.event === 'metadata') console.log('focus', ev.data.FocusFoM);
};
```

```bash
curl -sN http://raspberrypi.local:8080/api/events
```

## Capture

`POST /api/capture` with an optional body `{"quality": 1…100}` (missing or 0:
the configured `still.quality`) takes a snapshot from the live stream
without interrupting it ([HARDWARE.md §5](HARDWARE.md#5-snapshot)). The
response is **the JPEG itself**:

- `Content-Type: image/jpeg`
- `Content-Disposition: attachment; filename="IMG_YYYYMMDD_HHMMSS.jpg"`
- `X-Capture-Width`, `X-Capture-Height` (2028 × 1520)

Nothing is kept on the Pi: the file the helper writes (in RAM, see
`-captures`) is deleted as soon as camweb has read it, before the response is
sent. Save the response if you want to keep the image. Two captures within one
second are named `IMG_…_1.jpg`, `IMG_…_2.jpg`, and so on.

```bash
curl -s -X POST http://raspberrypi.local:8080/api/capture -d '{"quality": 90}' -o snap.jpg
```

## Video streams

All three share one camera pipeline; any number of clients can watch.
Clients that fall behind lose frames instead of building up delay.

### /stream

The page's stream: H.264 frames and the [events](#events) in one response.
The body is a sequence of messages, each a **16-byte header** plus payload
(big-endian):

```
offset size field
0      1    type     1 = H.264 access unit (one frame, Annex-B), 2 = event (JSON), 3 = keepalive (no payload)
1      1    flags    bit0 keyframe (IDR), bit1 contains SPS/PPS
2      2    reserved
4      4    payload length
8      8    ts       µs since the Unix epoch: when the helper handed the frame over (events: when sent)
16     N    payload
```

- The first messages are the latest sticky events (`state`, `camera`,
  `stream`, `metadata`, `stats`). Video starts at the next keyframe, which
  carries SPS/PPS; a new `stream` event means the encoder restarted (possibly
  with a new size).
- If the client's queue (30 messages, about 1 s) fills up, the server drops
  frames until the next keyframe and sends a `drop` event.
- A keepalive is sent after 2 s without other messages.
- `?video=0`: events only, no video (used by the MJPEG fallback page).
- Decoding in a browser: WebCodecs `VideoDecoder` with codec
  `avc1.PPCCLL` from the SPS (bytes 1–3) and Annex-B chunks (no
  `description`); see `internal/web/static/player.js`. WebCodecs needs HTTPS
  or `localhost`.

### /stream.h264

The raw H.264 elementary stream (Annex-B, High profile, SPS/PPS before every
keyframe), starting at a keyframe, for players and tools.

Play with mpv (low latency; `--untimed` shows each frame as soon as it
arrives instead of pacing it):

```bash
mpv --profile=low-latency --untimed --demuxer-lavf-format=h264 http://raspberrypi.local:8080/stream.h264
```

Play with ffplay:

```bash
ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 http://raspberrypi.local:8080/stream.h264
```

Record 60 s without re-encoding:

```bash
ffmpeg -f h264 -i http://raspberrypi.local:8080/stream.h264 -c copy -t 60 clip.mp4
```

**With HTTPS** (`-tls-cert`), use the `https://` address and either trust
your CA or skip the check:

| Tool | Trust your CA | Skip the check |
|---|---|---|
| mpv | `--tls-verify=yes --tls-ca-file=certs/ca.crt` | `--tls-verify=no` |
| ffplay, ffmpeg, ffprobe | `-tls_verify 1 -ca_file certs/ca.crt` | `-tls_verify 0` |

Trusting the CA works only if the address you use is one of the certificate's
names (`CERT_HOSTS` in `make cert`). For example:

```bash
mpv --tls-verify=no --profile=low-latency --untimed --demuxer-lavf-format=h264 https://raspberrypi.local:8443/stream.h264
```

```bash
ffplay -tls_verify 0 -fflags nobuffer -flags low_delay -framedrop -f h264 https://raspberrypi.local:8443/stream.h264
```

There are no timestamps in a raw stream: players take the frame rate from
the SPS timing information, which states the real frame rate
([HARDWARE.md §3.1](HARDWARE.md#31-why-the-encoders-assume-30-fps)). Note
that ffprobe's `r_frame_rate` for this stream shows twice the frame rate (the
H.264 tick rate); that is expected. After a stream-size change the stream continues with new
SPS/PPS.

### /stream.mjpeg

`multipart/x-mixed-replace; boundary=camweb-frame`, one complete JPEG per part
(`Content-Type: image/jpeg`, `Content-Length`). Works in a plain
`<img src="/stream.mjpeg">`, over HTTP too, and in most players.

- While at least one MJPEG client is connected, the Pi runs a second hardware
  encoder (`mjpeg.bitrate`, every `mjpeg.skip`-th frame); it stops after the
  last one leaves.
- Each client always gets the newest frame; a slow client skips frames.
- Several times the bandwidth of H.264.

Play it with mpv or ffplay (TLS options as for `/stream.h264`):

```bash
mpv --profile=low-latency --untimed http://raspberrypi.local:8080/stream.mjpeg
```

```bash
ffplay -fflags nobuffer -f mpjpeg http://raspberrypi.local:8080/stream.mjpeg
```

## Errors

Errors are JSON: `{"error": "message", "field": "controls.gain"}` (`field`
when one setting is at fault).

| Status | Meaning |
|---|---|
| 400 | invalid JSON, unknown field, or a value out of range (`field` names it) |
| 409 | the camera is busy with another operation |
| 503 | no camera helper connected, or the camera is not streaming yet |
| 504 | the camera did not answer in time |
| 500 | other camera or I/O error (also logged) |

## CORS

By default the API is same-origin only. `camweb -cors-origin <origin>` adds
`Access-Control-Allow-Origin: <origin>` to every response, answers preflight
`OPTIONS` requests (methods GET, POST; header Content-Type) and exposes the
capture headers. Use `*` to allow any origin, only on a trusted network.

## Example: a minimal client

Python, standard library only: switch to manual exposure, wait for the
metadata to reflect it, take a snapshot.

```python
import json, urllib.request

BASE = "http://raspberrypi.local:8080"

def post(path, body):
    req = urllib.request.Request(BASE + path, json.dumps(body).encode(),
                                 {"Content-Type": "application/json"})
    return urllib.request.urlopen(req)

print(json.load(post("/api/config", {"controls": {"auto_exposure": False, "shutter_us": 20000, "gain": 2}}))["applied"])

with urllib.request.urlopen(BASE + "/api/events") as es:
    for line in es:
        if line.startswith(b"data: "):
            ev = json.loads(line[6:])
            if ev["event"] == "metadata" and abs(ev["data"].get("ExposureTime", 0) - 20000) < 500:
                break

with post("/api/capture", {"quality": 95}) as r, open("snap.jpg", "wb") as f:
    f.write(r.read())
```
