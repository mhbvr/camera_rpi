# Hardware details and camera settings

What camweb does with the **Raspberry Pi 4** and the **HQ Camera (Sony
IMX477)**, and what every setting in the UI and in
[`/api/config`](REST_API.md#config) actually does. Which of this carries over
to other Pis and cameras: [HW_SPECIFIC.md](HW_SPECIFIC.md). Measurements:
[SPIKE.md](SPIKE.md).

Source links point to
[raspberrypi/libcamera @ 9fe1678](https://github.com/raspberrypi/libcamera/tree/9fe1678b8ad5fbae875942b55b58ade23014194c),
[raspberrypi/picamera2 @ ae3f32f](https://github.com/raspberrypi/picamera2/tree/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6)
and [raspberrypi/linux rpi-6.6.y](https://github.com/raspberrypi/linux/tree/bba53a117a4a5c29da892962332ff1605990e17a).
Tables quoted from the HQ camera's tuning file (`imx477.json`) are from that
commit; the file on a given Pi may differ (§8.4).

Contents:
1. [Platform](#1-platform)
2. [Sensor mode and streams](#2-sensor-mode-and-streams)
3. [Encoders](#3-encoders)
4. [Settings reference](#4-settings-reference)
5. [Snapshot](#5-snapshot)
6. [Metadata and actual values](#6-metadata-and-actual-values)
7. [Focus value (FocusFoM)](#7-focus-value-focusfom)
8. [Exposure: EV, frame rate, shutter, gain](#8-exposure-ev-frame-rate-shutter-gain)
9. [Lux](#9-lux)
10. [Metering](#10-metering)
11. [Image processing: brightness, contrast, saturation, sharpness, denoise](#11-image-processing-brightness-contrast-saturation-sharpness-denoise)
12. [White balance](#12-white-balance)
13. [Memory, buffers and latency](#13-memory-buffers-and-latency)

---

## 1. Platform

| Item | Used |
|---|---|
| Board | Raspberry Pi 4 (VC4 platform: BCM2711, Pi 4 ISP, hardware H.264 and MJPEG encoders) |
| Camera | HQ Camera (IMX477, 12.3 MP, 4056×3040) on CSI, manual-focus C/CS lens |
| OS | Raspberry Pi OS Bookworm or newer, 64-bit preferred |
| Camera stack | libcamera + **Picamera2** (`python3-picamera2`, preinstalled on desktop images; on Lite: `sudo apt install -y python3-picamera2 --no-install-recommends`) |
| Python | system `python3` with Picamera2 and numpy (pulled in by Picamera2); no pip packages |

Facts about the Pi 4 camera system that shape the design (Picamera2 manual,
confirmed by the spike):
- The ISP produces **up to two processed images per sensor frame**: **main**
  (RGB or YUV) and **lores**. On the Pi 4, lores **must be YUV** and **no
  larger than main**
  ([picamera2.py#L1026-L1027](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/picamera2.py#L1026-L1027)).
  The raw sensor frame can be delivered at the same time.
- main and lores are scaled from the same sensor crop and share flips and
  colour space, so lores needs **the same aspect ratio** as main or the
  picture is stretched.
- **One sensor mode drives every stream.** It sets the maximum frame rate and
  the largest image. Changing it needs a stop/configure/start.
- Encoding: hardware **H.264** (max 1920×1080, level 4.2) and hardware
  **MJPEG** through the V4L2 `bcm2835-codec` driver. Picamera2 uses them only
  on VC4
  ([encoders/__init__.py#L12-L23](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/encoders/__init__.py#L12-L23)).
  Still JPEGs are encoded **in software**.
- Runtime controls apply a few frames later without a restart (§4).

## 2. Sensor mode and streams

IMX477 sensor modes (spike 0.1; 8- and 10-bit variants also exist, with
higher frame rates):

| Sensor mode | Max fps | Use |
|---|---|---|
| 1332×990 12-bit | 101.7 | high fps, cropped field of view |
| 2028×1080 12-bit | 62.8 | 16:9, cropped |
| **2028×1520 12-bit (2×2 binned)** | **45.2** | **used: 4:3, full field of view** |
| 4056×3040 12-bit | 11.7 | full-resolution stills (not used) |

camweb uses **one mode, 2028×1520 12-bit, at 1–40 fps**. With the rules in §1
(lores ≤ main, same aspect ratio, H.264 ≤ 1080p) this fixes everything but the
stream size:

| Stream | Size | Used for |
|---|---|---|
| sensor mode | 2028×1520 | everything |
| main (YUV420) | 2028×1520 (3.1 MP) | snapshots |
| lores (YUV420) | **1440×1080** (default), 1024×768, 640×480 | H.264 stream and MJPEG fallback |

All sizes were applied unchanged by libcamera, with the full sensor crop
(spike 0.6). They are constants in `internal/camera/config.go`
(`SensorSize`, `SensorBitDepth`, `MainSize`, `StreamSizes`); the helper takes
explicit sizes and has no camera-specific code.

Picamera2 configuration, as done by the helper:
```python
cfg = picam2.create_video_configuration(
    main={"size": (2028, 1520), "format": "YUV420"},
    lores={"size": stream_size, "format": "YUV420"},
    sensor={"output_size": (2028, 1520), "bit_depth": 12},
    transform=Transform(hflip=h, vflip=v),       # camweb -hflip / -vflip
    buffer_count=buffer_count,
    controls={"FrameDurationLimits": (d, d)},    # d = 1e6 / fps
)
picam2.configure(cfg)
picam2.start_encoder(H264Encoder(bitrate=..., repeat=True, iperiod=gop, profile="high"), output, name="lores")
picam2.start()
```
- `repeat=True` puts SPS/PPS before every keyframe, so a viewer can join at
  any keyframe.
- The H.264 profile is fixed at `high`.
- Flips are startup flags (`-hflip`, `-vflip`; both = rotate 180°), not
  settings.
- Colour space: Rec709, limited range (spike 0.1). Snapshots expand it to full
  range (§5).

## 3. Encoders

**H.264** (hardware, on lores):
- One encoder callback = exactly one frame; every keyframe carries SPS/PPS
  (spike 0.2). The helper sends each frame as one message, so there is no
  frame-splitting delay.
- **The encoder's rate control assumes 30 fps**: delivered bitrate =
  requested × fps / 30 (spike 0.9, exact fit). The helper therefore hands the
  encoder **`bitrate × 30 / fps`**, capped at 25 Mbit/s, so `encoder.bitrate`
  is the delivered bitrate (confirmed within 1–5 %, spike 0.9 re-run). Because
  the compensation depends on the frame rate, **a frame-rate change restarts
  the encoder**. Why 30 (§3.1).

### 3.1 Why the encoders assume 30 fps

The encoders are fed frames by the application, one at a time, and are never
told the camera's frame rate unless the application says so:
1. **The kernel driver defaults to 30 fps.** The V4L2 `bcm2835-codec` driver
   (`/dev/video11`, which Picamera2 uses for both H.264 and MJPEG) sets the
   frame rate of every new encoder context to 30/1
   ([bcm2835-v4l2-codec.c#L3418-L3419](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/drivers/staging/vc04_services/bcm2835-codec/bcm2835-v4l2-codec.c#L3418-L3419)) and passes it, together
   with the bitrate, to the VideoCore firmware encoder in the port format
   ([#L936-L952](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/drivers/staging/vc04_services/bcm2835-codec/bcm2835-v4l2-codec.c#L936-L952)). A constant-bitrate encoder spends about
   bitrate / frame rate bits per frame, so at 40 fps it delivers 40/30 of the
   requested bitrate, at 15 fps half of it: exactly what spike 0.9 measured.
2. **Only `VIDIOC_S_PARM` changes it.** The driver's `vidioc_s_parm` stores the
   application's `timeperframe` and sends it to the firmware as
   `MMAL_PARAMETER_VIDEO_FRAME_RATE`
   ([#L2037-L2076](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/drivers/staging/vc04_services/bcm2835-codec/bcm2835-v4l2-codec.c#L2037-L2076)).
3. **Picamera2 calls `VIDIOC_S_PARM` only with `enable_sps_framerate=True`.**
   `H264Encoder(framerate=…)` alone does nothing to the encoder: the call is
   made only when `framerate` is set **and** `enable_sps_framerate=True`
   ([v4l2_encoder.py#L95-L103](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/encoders/v4l2_encoder.py#L95-L103);
   [h264_encoder.py#L27-L50](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/encoders/h264_encoder.py#L27-L50): *"The framerate can
   be reported in the sequence headers if enable_sps_framerate is set, but
   there's no guarantee that frames will be delivered to the codec at that
   rate!"*). That is why spike 0.9 saw no effect from `framerate=`.
4. **`MJPEGEncoder` has no frame-rate option at all**
   ([mjpeg_encoder.py#L12-L20](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/encoders/mjpeg_encoder.py#L12-L20)); Picamera2's
   comment says only H.264 supports the setting. With `frame_skip_count` the
   MJPEG encoder sees fps / skip frames per second but still budgets for 30.

Consequences:
- The compensation is a workaround for a missing setting, not an encoder
  limit. For H.264 the proper fix is `H264Encoder(framerate=fps,
  enable_sps_framerate=True)` with the requested bitrate; `spike/m0_spike.py
  limits` has rows (`s_parm`) that measure it, and the helper keeps the
  compensation until they confirm it. For MJPEG the compensation stays (no
  Picamera2 option).
- Side effect of the default: the driver always writes timing information
  into the H.264 SPS ([#L2792-L2798](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/drivers/staging/vc04_services/bcm2835-codec/bcm2835-v4l2-codec.c#L2792-L2798)), so without `S_PARM`
  the stream claims 30 fps. Browsers ignore it; tools reading
  `/stream.h264` may use it, hence `-framerate` in the REST_API.md examples.
- A C++ helper using V4L2 directly would simply call `VIDIOC_S_PARM`.
- CPU (helper process): about 11 / 22 / 43 % of one core at 15 / 30 / 40 fps,
  independent of bitrate (spike 0.9).

**MJPEG** (hardware, on lores, only while an MJPEG fallback viewer is
connected):
- Runs next to the H.264 encoder on the same stream. Picamera2's
  `frame_skip_count` encodes every n-th frame (`mjpeg.skip`).
- Same 30 fps assumption: the helper hands it `bitrate × 30 / (fps / skip)`,
  capped at 25 Mbit/s. Delivered bitrate is within about ±10 % of
  `mjpeg.bitrate` (spike 0.10 re-run); very low bitrates hit a quality floor.
- H.264 keeps its full frame rate with 0 dropped frames; about +5 % CPU
  (spike 0.10). Frames are 25–75 KB at 1440×1080.

**Still JPEG** (software, Picamera2/simplejpeg or Pillow): about 94 ms for a
2028×1520 frame, in a worker thread (spike 0.5).

## 4. Settings reference

All settings live in one JSON document ([`GET/POST /api/config`](REST_API.md#config)),
persisted in `<data>/config.json` and re-applied whenever the helper
(re)connects. Ranges are in [`GET /api/schema`](REST_API.md#schema) and in
`internal/camera/config.go` (`Limits`); the mapping to libcamera controls is in
`internal/camera/controls.go`.

| Setting | Range (default) | libcamera / Picamera2 | Applied by | Details |
|---|---|---|---|---|
| `pipeline.stream_size` | 1440×1080, 1024×768, 640×480 (1440×1080) | lores size | reconfigure | §2 |
| `pipeline.buffer_count` | 2–12 (6) | `buffer_count` | reconfigure | §13 |
| `encoder.bitrate` | 0.25–25 Mbit/s (6) | H264Encoder bitrate, compensated | encoder restart | §3 |
| `encoder.gop` | 1–300 frames (30) | `iperiod` | encoder restart | §3 |
| `controls.fps` | 1–40 (30) | `FrameDurationLimits` (d, d) | live + encoder restart | §8 |
| `controls.auto_exposure` | bool (true) | `ExposureTimeMode`, `AnalogueGainMode` (or `AeEnable`) | live | §8 |
| `controls.shutter_us` | 31–1 000 000 µs (10 000) | `ExposureTime` (manual) | live | §8 |
| `controls.gain` | 1–22 (1) | `AnalogueGain` (manual) | live | §8 |
| `controls.ev` | −8…8 (0) | `ExposureValue` (auto) | live | §8 |
| `controls.metering` | centre, spot, matrix (centre) | `AeMeteringMode` | live | §10 |
| `controls.exposure_mode` | normal, short, long (normal) | `AeExposureMode` | live | §8 |
| `controls.auto_wb` | bool (true) | `AwbEnable` | live | §12 |
| `controls.awb_mode` | auto, incandescent, tungsten, fluorescent, indoor, daylight, cloudy (auto) | `AwbMode` | live | §12 |
| `controls.wb_gain_r`, `wb_gain_b` | 0.1–32 (1.5) | `ColourGains` (manual) | live | §12 |
| `controls.brightness` | −1…1 (0) | `Brightness` | live | §11 |
| `controls.contrast` | 0–32, UI 0–4 (1) | `Contrast` | live | §11 |
| `controls.saturation` | 0–32, UI 0–4 (1) | `Saturation` | live | §11 |
| `controls.sharpness` | 0–16, UI 0–8 (1) | `Sharpness` | live | §11 |
| `controls.denoise` | auto, off, fast, high_quality, minimal (auto) | `NoiseReductionMode` | live | §11 |
| `still.quality` | 1–100 (95) | JPEG quality | per capture | §5 |
| `mjpeg.bitrate` | 1–25 Mbit/s (8) | MJPEGEncoder bitrate, compensated | MJPEG encoder restart | §3 |
| `mjpeg.skip` | 1–10 (2) | `frame_skip_count` | MJPEG encoder restart | §3 |

What a change costs (the server diffs old and new config and does the least
disruptive thing, `internal/camera/config.go` `Diff`):

| Change | Mechanism | Stream impact |
|---|---|---|
| exposure, white balance, image controls | `set_controls` | none; takes effect after ~10 frames with 6 buffers (spike 0.7) |
| bitrate, GOP, frame rate (also a manual shutter longer than the frame time, which stretches the frame time) | `set_controls` if needed, then encoder restart | new keyframe, short gap |
| stream size, buffer count | stop → configure → start inside the helper | short gap (a few hundred ms) |
| MJPEG settings | MJPEG encoder restart, only while MJPEG viewers are connected | none on H.264 |
| helper crash | camweb restarts the helper | about 1–3 s |

The camera's own limits come from libcamera's control ranges in the helper's
`hello`: the server rejects a gain or shutter above the camera's maximum
(`AnalogueGain` 22.26 on the HQ camera). Mode-dependent ranges such as
`FrameDurationLimits` are not used, because Picamera2 reports them for the last
configured mode (spike 0.1).

## 5. Snapshot

**Capture** (`POST /api/capture`) takes a JPEG **from the running main stream
without interrupting the video**:
- The helper takes the next completed request, copies the main buffer
  (~4.6 MB YUV420, 13 ms mean) and releases it at once. The H.264 stream keeps
  running: 0 dropped frames while capturing (spike 0.4).
- JPEG encoding runs in a worker thread (~94 ms). It holds the Python GIL and
  adds jitter to the encoder output: callback interval p95 35 → 47 ms during
  snapshots (spike 0.5).
- Resolution 2028×1520. Exposure and white balance are those of the live
  stream (same frame). There is no separate still exposure.
- The YUV is limited range (Rec709); the helper expands it to full range
  before encoding (with 32-bit arithmetic: 16-bit overflowed and turned bright
  areas black).
- The file is written as `<name>.jpg.tmp`, renamed, returned to the client
  and kept in `<data>/captures/IMG_YYYYMMDD_HHMMSS.jpg`.

Not implemented: 12 MP capture (needs a mode switch and a short pause) and DNG
raw ([HELPER_API.md §7](HELPER_API.md#7-future-extensions-reserved-not-in-v1)).

## 6. Metadata and actual values

A Picamera2 `post_callback` samples per-frame metadata at about 2 Hz and
sends it as the `metadata` event ([REST_API.md](REST_API.md#events)):
`ExposureTime`, `AnalogueGain`, `DigitalGain`, `ColourGains`,
`ColourTemperature`, `Lux`, `FrameDuration`, `FocusFoM`, `AeState`,
`SensorTimestamp` (when present).

The UI shows them under *Actual values*. **Lock current** copies the current
auto exposure and white-balance values into the manual settings (it only
posts a config; any client can do the same).

## 7. Focus value (FocusFoM)

The UI's **Focus** number is libcamera's `FocusFoM` metadata, shown raw: a
**sharpness score computed by the image processor**, not something the lens
reports. The HQ camera has no autofocus. To focus, turn the lens ring until the
number peaks.

How the Pi 4 computes it:
1. The ISP runs a fixed focus (contrast) filter and accumulates its output in
   12 regions: *"Focus measure - accumulated output of the focus filter"*
   ([bcm2835-isp.h#L310-L324](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/include/uapi/linux/bcm2835-isp.h#L310-L324),
   [`FOCUS_REGIONS 12`](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/include/uapi/linux/bcm2835-isp.h#L278)).
2. On the Pi 4 the regions form a 4 × 3 grid
   ([controller.cpp#L40](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/controller.cpp#L40)).
3. libcamera takes the second filter's sum over pixels above the threshold,
   divided by 1000, per region
   ([vc4.cpp#L278-L282](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/vc4.cpp#L278-L282)).
4. `FocusFoM` is the average of the central regions only: the middle row, two
   middle columns, roughly the **middle third of the height and middle half of
   the width**
   ([ipa_base.cpp#L1641-L1663](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1641-L1663)).

libcamera's definition: *"A larger FocusFoM value indicates a more in-focus
frame … not necessarily aimed at providing a way to implement a focus algorithm
by the application, rather an indication of how in-focus a frame is"*
([control_ids_core.yaml#L657-L668](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/libcamera/control_ids_core.yaml#L657-L668)).

**Reading it:**
- Higher is sharper. No unit, no absolute scale: it is a sum, not divided by
  the pixel count, so it depends on scene detail, brightness and exposure.
- Compare values only for the same scene and settings.
- Sensor noise at high gain raises it: focus in good light with exposure
  locked (*Lock current*).

**Not configurable:** the ISP driver exposes no focus-statistics settings
([bcm2835-isp.h#L18-L35](https://github.com/raspberrypi/linux/blob/bba53a117a4a5c29da892962332ff1605990e17a/include/uapi/linux/bcm2835-isp.h#L18-L35));
the grid, filter and centre averaging are fixed in code; `imx477.json` has no
focus section; the autofocus window controls (`AfWindows`, `AfMetering`) exist
only with a movable lens
([ipa_base.cpp#L105-L115](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L105-L115),
[#L278-L280](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L278-L280)).

Possible extensions: `StatsOutputEnable` delivers the raw per-region
statistics
([control_ids_rpi.yaml#L10-L31](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/libcamera/control_ids_rpi.yaml#L10-L31)),
from which the helper could build a 4 × 3 sharpness map; or the helper could
compute its own metric over a chosen region.

## 8. Exposure: EV, frame rate, shutter, gain

### 8.1 Auto mode
Raspberry Pi's auto-exposure algorithm (`rpi.agc`), every frame:
1. `ExposureValue` (EV) becomes a multiplier 2^EV
   ([ipa_base.cpp#L1083-L1099](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1083-L1099)).
2. The brightness target from the tuning file (`y_target`, ≈ 0.16–0.17 of
   full scale for the HQ camera) is multiplied by 2^EV and **capped at 0.9**
   ([agc_channel.cpp#L693-L711](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L693-L711)),
   so EV above about **+2.4** has no further effect.
3. The needed total exposure (shutter × gain) is **capped at max shutter × max
   gain** of the selected exposure profile; max shutter is the profile's last
   step, limited by the frame time
   ([#L763-L786](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L763-L786)).
4. The frame time caps the shutter: `FrameDurationLimits` (camweb sends
   min = max = 1/fps) sets the longest exposure the sensor allows
   ([ipa_base.cpp#L1780-L1810](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1780-L1810),
   [agc_channel.cpp#L972-L982](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L972-L982)).
5. The exposure is split by walking the profile's steps: shutter to the next
   step, then gain to the next step. Analogue gain is capped at the sensor
   maximum (22.26); digital gain makes up the rest, between 1 and
   `max_digital_gain` (default 4)
   ([#L881-L956](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L881-L956)).

Exposure profiles (`controls.exposure_mode`), HQ camera (`imx477.json`,
`rpi.agc → exposure_modes`):

| Profile | Shutter steps (µs) | Gain steps |
|---|---|---|
| normal | 100, 10000, 30000, 60000, 66666 | 1, 2, 4, 6, 16 |
| short | 100, 5000, 10000, 20000, 66666 | 1, 2, 4, 6, 16 |
| long | 100, 10000, 30000, 60000, 120000 | 1, 2, 4, 6, 16 |

**Why EV can do nothing:** in a dark scene at 30 fps auto exposure sits at
33 ms × gain 16, the cap from steps 3–4 (spike 0.7: `ExposureTime=33013
AnalogueGain=16.0`). EV raises a target that is already out of reach. To get a
brighter image: lower the frame rate (15 fps allows the normal profile's
66.7 ms), use the "long" profile (120 ms) at ≤ 8 fps, or switch to manual.

### 8.2 Manual mode
- `AeEnable` is a wrapper for `ExposureTimeMode`/`AnalogueGainMode`
  ([ipa_base.cpp#L1103-L1112](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1103-L1112));
  camweb sends the two modes when the camera has them (spike 0.1).
- Manual values are ignored while auto is on
  ([#L979-L997](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L979-L997),
  [#L1003-L1020](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1003-L1020)).
- **EV, metering and profile have no effect**: the exposure is simply
  shutter × gain
  ([agc_channel.cpp#L765-L767](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L765-L767)).
  The UI disables them.
- The shutter is still clamped by the frame time. camweb stretches the frame
  time when the manual shutter is longer (`FrameDurationUS` in
  `internal/camera/controls.go`), which lowers the frame rate and restarts the
  encoder.
- libcamera accepts manual gain up to analogue max × max digital gain (≈ 89)
  ([#L985-L1000](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L985-L1000));
  camweb stops at 22.

### 8.3 Reading the actual values
Per-frame metadata: `ExposureTime`, `AnalogueGain`, `DigitalGain`,
`FrameDuration`, `AeState` (0 Idle, 1 Searching, 2 Converged)
([ipa_base.cpp#L1590-L1619](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1590-L1619)).
- There is **no "limit reached" flag**.
- `Converged` only means the values stopped changing: *"we don't know the
  exposure/gain limits of the sensor, so the values we keep requesting may be
  unachievable"*
  ([agc_channel.cpp#L527-L564](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L527-L564)).
  A stuck dark scene reports Converged.
- The UI warns in auto mode when **shutter ≥ 95 % of the frame time**: from
  then on only gain can brighten the image, up to the profile's last gain step;
  when that is reached, EV has no effect. The UI cannot see the profile's own
  shutter limit, which can be below the frame time at low frame rates.

### 8.4 Which tuning file is used
camweb does not read the tuning file; the ⓘ popups explain behaviour in
general and name the parameter that sets the values (for example
`rpi.agc → exposure_modes`). libcamera logs *"Using tuning file …"* (in the
camweb log, helper lines are prefixed `[helper]`) and picks the file in this
order
([ipa_proxy.cpp#L33-L94](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/libcamera/ipa_proxy.cpp#L33-L94)):
1. `LIBCAMERA_RPI_TUNING_FILE`;
2. `LIBCAMERA_IPA_CONFIG_PATH` dirs + `/rpi/vc4/<sensor>.json`;
3. the system dirs, usually `/usr/share/libcamera/ipa/rpi/vc4/imx477.json`;
4. `uncalibrated.json` as a fallback.

Picamera2 clears `LIBCAMERA_RPI_TUNING_FILE` when it opens the camera without
an explicit tuning
([picamera2.py#L336-L353](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/picamera2.py#L336-L353)).

## 9. Lux

*Lux* is libcamera's `Lux` metadata, *"an estimate of the current illuminance
level in lux"*
([control_ids_core.yaml#L508-L514](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/libcamera/control_ids_core.yaml#L508-L514)),
computed from the image; the camera has no light sensor. `rpi.lux` compares
the frame with a reference recorded during calibration
([lux.cpp#L84-L105](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/lux.cpp#L84-L105)):

```
lux = reference_lux × (reference_shutter / shutter) × (reference_gain / analogue_gain)
      × (reference_aperture / aperture)² × (Y / reference_Y) / sensitivity
```

- **Reference values** (HQ camera): lux 830, shutter 27242 µs, gain 1.0,
  aperture 1.0, Y 17755
  ([imx477.json#L15-L22](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/data/imx477.json#L15-L22)).
- **Y** is the average brightness of the whole frame (R/G/B sums with
  white-balance gains, weighted 0.299 / 0.587 / 0.114), independent of the
  metering mode
  ([vc4.cpp#L271-L275](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/vc4.cpp#L271-L275)).
- Digital gain is not used (statistics are taken before it). The HQ lens
  reports no aperture, so that term is 1
  ([lux.cpp#L97](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/lux.cpp#L97));
  sensitivity is 1.0
  ([cam_helper.cpp#L169-L177](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/cam_helper/cam_helper.cpp#L169-L177)).
  Before the first statistics the value is 400
  ([lux.cpp#L22-L31](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/lux.cpp#L22-L31)).

**Reading it:** absolute only for the (undocumented) calibration lens and
f-stop. With a C/CS lens treat it as a **relative** brightness indicator for
one lens setup: one stop smaller aperture halves it. It measures reflected
light, assumes an average scene, and is less reliable with clipped highlights
or at maximum gain. Auto exposure chooses `y_target` by lux
([agc_channel.cpp#L712-L722](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L712-L722))
and auto white balance its priors
([awb.cpp#L341-L345](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb.cpp#L341-L345)),
so a lens much darker or brighter than the calibration lens shifts both
slightly.

## 10. Metering

Metering decides **which parts of the image** auto exposure uses to judge
brightness (auto mode only). The Pi 4 ISP collects statistics in **15 fixed
zones**
([Camera Algorithm and Tuning Guide §5.9.3, Figure 9](https://datasheets.raspberrypi.com/camera/raspberry-pi-camera-guide.pdf#page=33);
sizes approximate):

```
+----+------------------------+----+
| 11 |           9            | 12 |
+----+----+-------------+-----+----+
|    |    |      3      |     |    |
|    |    +----+---+----+     |    |
| 7  | 5  | 1  | 0 | 2  |  6  | 8  |
|    |    +----+---+----+     |    |
|    |    |      4      |     |    |
+----+----+-------------+-----+----+
| 13 |           10           | 14 |
+----+------------------------+----+
```

Each mode is a list of 15 weights (`rpi.agc → metering_modes → <mode> →
weights`, zone order 0–14). libcamera multiplies each zone's sums *and* pixel
count by its weight
([vc4.cpp#L251-L261](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/vc4.cpp#L251-L261)),
so the brightness driven to the target is a weighted pixel average
([agc_channel.cpp#L633-L691](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/agc_channel.cpp#L633-L691)):
`Y = Σ wᵢ·(sum of pixels in zone i) / Σ wᵢ·(pixel count of zone i)`.

HQ camera weights
([imx477.json#L206-L229](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/data/imx477.json#L206-L229)):

| Setting | libcamera / tuning name | Weights (zones 0…14) | Effect |
|---|---|---|---|
| centre | `CentreWeighted` / `centre-weighted` | 3 3 3 · 2 2 2 2 · 1 1 1 1 · 0 0 0 0 | centre row ×3, ring ×2, edges ×1, corners ignored |
| spot | `Spot` / `spot` | 2 1 1 · 0 … 0 | only the three small central zones |
| matrix | `Matrix` / `matrix` | all 1 | whole-frame average |

`AeMeteringMode` maps to these names in
[ipa_base.cpp#L750-L755](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L750-L755).
`custom` is not defined in the HQ tuning, so it is not offered.

Metering does **not** change: the histogram constraints (`rpi.agc →
constraint_modes`, which count every pixel once; the HQ default requires the
brightest 2 % to reach at least 0.3 of full scale, which can raise exposure in
dim scenes), the lux estimate (§9), or the limits (§8). The zone drawing in the
UI is fixed in `internal/web/static/app.js` (`METERING_ZONES`).

## 11. Image processing: brightness, contrast, saturation, sharpness, denoise

These change how the ISP renders the image, live, for stream and snapshots,
in both exposure modes. They **do not change exposure**.

| Setting | libcamera algorithm | What it does |
|---|---|---|
| brightness | `rpi.contrast` | adds `brightness × 65536` to the tone (gamma) curve output |
| contrast | `rpi.contrast` | scales the curve around mid-grey: `(y − 32768) × contrast + 32768` |
| saturation | `rpi.ccm` | scales chroma inside the colour-correction matrix |
| sharpness | `rpi.sharpen` | scales the ISP sharpening strength, limit and threshold |
| denoise | `rpi.sdn` + ISP colour denoise | selects spatial and colour noise reduction |

**Brightness and contrast** are applied in one step at the end of the tone
curve
([contrast.cpp#L143-L190](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/contrast.cpp#L143-L190),
controls mapped in
[ipa_base.cpp#L1255-L1283](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L1255-L1283)).
Values are clipped to 16 bits, so brightness ±0.2 lifts blacks or crushes
shadows: a level shift after gamma, not exposure. Before that, the HQ tuning
enables **adaptive contrast enhancement** (`ce_enable: 1`,
[imx477.json#L645](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/data/imx477.json#L645)):
the 1 % histogram point is moved towards 1.5 % of full scale (by at most 500 of
65536), the 95 % point towards 95 % (by at most 2000), the median stays
([contrast.cpp#L41-L141](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/contrast.cpp#L41-L141)).
camweb cannot turn this off.

**Saturation:** the CCM for the current colour temperature (6 matrices from
2850 K to 7600 K in the HQ tuning) is converted to Y/Cb/Cr, Cb and Cr are
multiplied by the saturation, and it is converted back
([ccm.cpp#L130-L182](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/ccm.cpp#L130-L182)).
0 gives greyscale; luma is unchanged.

**Sharpness:** for user strength *s* the ISP gets strength = 1.0 × *s* / m,
limit = 0.5 × √*s* / m, threshold = 0.75 × m / √*s*
([sharpen.cpp#L52-L84](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/sharpen.cpp#L52-L84);
base values
[imx477.json#L748-L752](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/data/imx477.json#L748-L752)),
where m = √(binX × binY) = 2 for the 2×2-binned mode
([ipa_base.cpp#L624](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/common/ipa_base.cpp#L624)).
*s* = 0 disables sharpening. Sharpening also raises bitrate and visible noise.

**Denoise:** `NoiseReductionMode` selects a Pi 4 denoise mode
([vc4.cpp#L293-L325](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/vc4.cpp#L293-L325))
driving two ISP blocks
([vc4.cpp#L454-L490](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/vc4.cpp#L454-L490)):

| Setting | `NoiseReductionMode` | spatial (SDN) | colour (CDN) |
|---|---|---|---|
| off | Off | off | off |
| minimal | Minimal | on | off |
| fast, auto | Fast | on | fast |
| high_quality | HighQuality | on | high quality |
| (not offered) | ZSL | on | high quality |

- SDN thresholds come from the sensor noise model × `deviation` 3.2, with
  `strength` 0.75 (defaults; the HQ tuning's `rpi.sdn` is empty)
  ([sdn.cpp#L38-L68](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/sdn.cpp#L38-L68)).
  The noise model scales with √(analogue gain)
  ([noise.cpp#L68-L76](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/noise.cpp#L68-L76)),
  so denoise strengthens at high gain.
- **auto = Picamera2's video default, Fast**
  ([picamera2.py#L953-L957](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/picamera2.py#L953-L957)).
  camweb sends Fast explicitly, so switching back to auto takes effect.
- Fast colour denoise normally uses the ISP's second output at a quarter of
  the main size
  ([pipeline vc4.cpp#L680-L712](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/libcamera/pipeline/rpi/vc4/vc4.cpp#L680-L712)).
  camweb uses that output for the stream (lores), and how fast CDN behaves then
  is not documented: compare off / fast / high quality at high gain.

## 12. White balance

White balance multiplies red and blue by **colour gains** (green = 1) so
neutral objects come out neutral; the CCM (§11) is then chosen for the
resulting colour temperature.

**Auto (`rpi.awb`, Bayesian search):**
1. Statistics from **16 × 12 zones**
   ([controller.cpp#L38](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/controller.cpp#L38));
   zones with fewer than 16 pixels counted or green below 32 are ignored
   ([awb.cpp#L373-L407](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb.cpp#L373-L407)).
2. Coarse search over colour temperatures from the mode's `lo` to `hi`: the
   calibrated r/b for each temperature (`ct_curve`) is scored by how far the
   zones would be from grey, minus the **prior** for that temperature at the
   current lux
   ([awb.cpp#L409-L425](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb.cpp#L409-L425),
   [awb_bayes.cpp#L215-L285](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb_bayes.cpp#L215-L285)).
3. Fine search around the best point, including small steps off the curve
   (`transverse_pos` 0.0238, `transverse_neg` 0.0443) for green/magenta tints
   ([#L287-L367](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb_bayes.cpp#L287-L367)).
4. Gains = `sensitivity` (1.05) / r and / b
   ([#L369-L413](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb_bayes.cpp#L369-L413)),
   smoothed with `speed` 0.05, searched every 10 frames
   ([awb.cpp#L71-L78](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb.cpp#L71-L78)):
   changes settle over a few seconds.

Modes (`controls.awb_mode`), HQ camera
([imx477.json#L42-L136](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/vc4/data/imx477.json#L42-L136),
`rpi.awb → modes → lo/hi`):

| Mode | Colour temperature range (K) |
|---|---|
| auto | 2500 – 8000 |
| incandescent | 2500 – 3000 |
| tungsten | 3000 – 3500 |
| fluorescent | 4000 – 4700 |
| indoor | 3000 – 5000 |
| daylight | 5500 – 6500 |
| cloudy | 7000 – 8600 |

Priors by lux: 0 lux favours ≤ 3000 K, 800 lux ≥ 6000 K, 1500 lux peaks at
6000–6500 K. The CT curve is calibrated from 2360 K to 9505 K.

**Manual:** `ColourGains` (r, b) sets the gains and turns auto off; the colour
temperature is estimated back from the curve so the CCM still matches
([awb.cpp#L209-L230](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb.cpp#L209-L230)).
*Lock current* copies the current auto gains. libcamera also accepts a manual
`ColourTemperature`
([awb.cpp#L232-L248](https://github.com/raspberrypi/libcamera/blob/9fe1678b8ad5fbae875942b55b58ade23014194c/src/ipa/rpi/controller/rpi/awb.cpp#L232-L248));
camweb does not offer it.

## 13. Memory, buffers and latency

- Camera buffers come from **CMA** (320 MB by default on Pi 4 models with more
  than 1 GB RAM). One buffer set of 2028×1520 + 1440×1080 YUV420 is about
  7 MB, so the default 6 buffers are comfortable. On "Failed to allocate
  buffers", lower `pipeline.buffer_count` or raise CMA in
  `/boot/firmware/config.txt` (`dtoverlay=vc4-kms-v3d,cma-512`).
- More buffers add pipeline depth: a manual exposure change took 10 frames
  with 6 buffers (spike 0.7).
- Camera-side latency (spike 0.3, dark scene, 33 ms exposure): sensor
  timestamp → request complete 35 ms p50; → H.264 encoder output 53 ms p50
  (73 ms p95); → socket < 1 ms. The `stats` event reports the helper's
  measured sensor → socket latency (`latency_ms`). End-to-end budget:
  [ARCHITECTURE.md §7](ARCHITECTURE.md#7-latency).
