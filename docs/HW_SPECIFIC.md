# What is hardware-specific

camweb was built and measured only on a **Raspberry Pi 4 with the HQ Camera
(IMX477)**. This page sorts the code into layers by how much it depends on
that hardware, so you can tell what carries over to other Raspberry Pis and
cameras and what has to change. Nothing here has been tested on other
hardware; statements about other boards come from Picamera2's source
([raspberrypi/picamera2 @ ae3f32f](https://github.com/raspberrypi/picamera2/tree/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6))
and Raspberry Pi's documentation.

## Summary

| Part | Depends on | Other Pi (VC4: Pi 3, Zero 2 W, CM4) | Pi 5 | Other Pi camera |
|---|---|---|---|---|
| Go server, REST API, hub, MJPEG/H.264 fan-out | nothing (any Linux, any CPU) | ✅ | ✅ | ✅ |
| Browser page and player | a browser | ✅ | ✅ | ✅ (UI texts mention the HQ camera) |
| Helper protocol ([HELPER_API.md](HELPER_API.md)) | nothing | ✅ | ✅ | ✅ |
| `camhelper.py` (Picamera2 calls) | Picamera2 + libcamera | ✅ expected | ⚠️ runs, but software encoders (below) | ✅ with new sizes |
| libcamera control mapping | libcamera's standard controls | ✅ | ✅ | ✅ (some controls may be missing) |
| Hardware H.264 / MJPEG encoders and their rate control | **VC4** | ✅ | ❌ software encoders | ✅ |
| Sensor mode, main and stream sizes, fps and gain limits | **IMX477** | ✅ | ✅ | ❌ change constants |
| Explanations and tables (tuning values, metering zones, focus grid) | **IMX477 tuning on VC4** | mostly ✅ | ❌ different statistics | ❌ different tuning |

## 1. Hardware-independent

- **Go server** (`cmd/camweb`, `internal/`): standard library only, built with
  `CGO_ENABLED=0`, runs on any Linux (and `make run-fake` runs it on a laptop).
  It knows nothing about the camera except the constants in
  `internal/camera/config.go` (§4).
- **REST API, streams, hub, backpressure, MJPEG fan-out, TLS, CORS.**
- **Web page and player** (WebCodecs H.264 with automatic MJPEG fallback).
- **Helper protocol**: JSON-RPC 2.0 + framed video over Unix sockets. A helper
  for a different camera stack (a C++ libcamera helper, a V4L2/ffmpeg helper
  for a USB webcam, …) can be written against it without touching the server.
  `cmd/fakecam` is such a helper with no camera at all.

## 2. Raspberry Pi camera stack (Picamera2 + libcamera)

`camhelper/camhelper.py` uses only generic Picamera2 calls
(`create_video_configuration`, `start_encoder`, `capture_request`,
`set_controls`, metadata callbacks). It has no sensor- or board-specific code;
sizes and controls come from the server.

- **Control names** (`ExposureTime`, `AnalogueGain`, `AwbMode`,
  `NoiseReductionMode`, …) are libcamera's standard controls, the same on all
  Raspberry Pi cameras and both Pi platforms. The helper rejects controls the
  camera does not list, and the server leaves out controls the camera does not
  report. Auto/manual exposure uses `ExposureTimeMode`/`AnalogueGainMode`
  (libcamera with these controls is required), and enum values come from the
  camera's `hello`.
- **Metadata** (`Lux`, `FocusFoM`, `ColourTemperature`, …) is produced by
  Raspberry Pi's libcamera algorithms on every Pi camera.
- **Tuning parameter names** shown in the UI popups (`rpi.agc →
  exposure_modes`, `rpi.awb → modes`, …) are Raspberry Pi algorithm names used
  by the tuning files of all Pi cameras; the values differ per sensor.

## 3. Pi 4 (VC4 platform) specific

Picamera2 distinguishes two platforms: **VC4** (Pi 4 and earlier: Pi 3,
Zero 2 W, CM3/CM4) and **PiSP** (Pi 5, CM5). These parts depend on VC4:

| What | Why it matters | Where |
|---|---|---|
| **Hardware H.264 and MJPEG encoders** | Picamera2 uses the V4L2 hardware encoders only on VC4; on Pi 5 `H264Encoder`/`MJPEGEncoder` are software libav encoders ([encoders/__init__.py#L12-L23](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/encoders/__init__.py#L12-L23)). The helper's calls are compatible, but encoding then costs CPU, and running H.264 + MJPEG in parallel is no longer nearly free | `camhelper.py` `_start_encoder`, `set_mjpeg` |
| **30 fps rate-control default** | the VC4 `bcm2835-codec` driver assumes 30 fps unless told the frame rate ([HARDWARE.md §3.1](HARDWARE.md#31-why-the-encoders-assume-30-fps)). The helper tells it (`framerate` + `enable_sps_framerate`, and the inherited attributes for MJPEG; Picamera2 ≥ 0.3.19). Other encoders: check with `spike/m0_spike.py limits mjpeg` that delivered = requested | `camhelper.py` `make_h264_encoder`, `make_mjpeg_encoder` |
| **H.264 ≤ 1920×1080** | hardware encoder limit, one reason for the stream sizes | `StreamSizes` |
| **lores must be YUV, ≤ main** | VC4 rule ([picamera2.py#L1026-L1027](https://github.com/raspberrypi/picamera2/blob/ae3f32ff8c80417f51d7e041dfbc1a71f9ee43d6/picamera2/picamera2.py#L1026-L1027)); camweb always uses YUV420, which also works on Pi 5 | helper `configure` |
| **15 metering zones**, **4 × 3 focus regions**, 16 × 12 AWB zones | VC4 ISP statistics layout; the UI's zone drawing and the focus explanation describe it. Pi 5's ISP has a different statistics layout | `app.js` `METERING_ZONES`, HARDWARE.md §7, §10 |
| **Denoise mapping** (SDN + CDN modes, CDN on the second ISP output) | VC4 ISP blocks; Pi 5 has different denoise hardware | HARDWARE.md §11, UI denoise line |
| **Tuning file location** `…/ipa/rpi/vc4/<sensor>.json` | Pi 5 uses `…/rpi/pisp/` | HARDWARE.md §8.4, UI popups |
| **CMA memory** (320 MB default, buffer count) | board memory; Zero 2 W and 1 GB boards have less | `pipeline.buffer_count` |

**Other VC4 boards** (Pi 3, Zero 2 W, CM4) have the same ISP and encoders, so
everything should work as on the Pi 4; they have less CPU and memory (lower
`buffer_count` if buffer allocation fails).

**Pi 5:** expected to run with software encoding: `picamera2.encoders`
substitutes libav encoders for `H264Encoder` and `MJPEGEncoder`. They cost
CPU, especially H.264 and MJPEG together. The helper passes them the frame
rate as an argument (they have no `enable_sps_framerate` or
`_enable_framerate`); whether libav then delivers the requested bitrate is
unmeasured. The explanations of metering zones, focus regions and denoise
would be wrong. Not tested.

## 4. HQ camera (IMX477) specific

Concentrated in `internal/camera/config.go` and the explanatory texts:

| What | Value for the HQ camera | Where |
|---|---|---|
| Sensor mode | 2028×1520 12-bit, 2×2 binned, full field of view, max 45.19 fps | `SensorSize`, `SensorBitDepth` |
| Snapshot size (main) | 2028×1520 | `MainSize` |
| Stream sizes (lores, same 4:3 aspect as main) | 1440×1080, 1024×768, 640×480 | `StreamSizes` |
| Frame-rate limit | 40 fps (below the mode's 45.19) | schema field `controls.fps` (`internal/camera/schema.go`) |
| Gain limit | 22 (sensor analogue maximum 22.26, also checked against `hello`) | schema field `controls.gain` |
| Exposure profiles, AWB modes, metering modes offered | those defined in `imx477.json` (no `custom`) | schema fields `controls.exposure_mode`, `controls.awb_mode`, `controls.metering` |
| Tables in HARDWARE.md (profiles, AWB ranges, metering weights, lux reference, sharpening, contrast enhancement) | from `imx477.json` | HARDWARE.md §8–§12 |
| Manual-focus C/CS lens: the Focus readout is a focusing aid; no autofocus controls | — | UI Focus, HARDWARE.md §7 |

At startup camweb logs a warning if the camera does not report the configured
sensor mode (`camera: WARNING: sensor mode … not reported by camera`).

## 5. Porting checklist

To use another Raspberry Pi camera:
1. List its modes: `rpicam-hello --list-cameras`, or
   `python3 spike/m0_spike.py info`.
2. Pick one mode for everything and set `SensorSize`, `SensorBitDepth` and
   `MainSize` (usually the mode size) in `internal/camera/config.go`.
3. Set `StreamSizes` to sizes ≤ main, with **the same aspect ratio** as main,
   and ≤ 1920×1080 on VC4. A 16:9 camera (for example Camera Module 3, IMX708)
   needs 16:9 stream sizes; the bundled page and `/api/schema` pick them up
   automatically.
4. Adjust the schema in `internal/camera/schema.go` (`controls.fps` below the
   mode's maximum, `controls.gain`, `controls.shutter_us`, and the enum values)
   to what the camera and its tuning file define. Validation, `/api/schema`
   and the page's form all follow it.
5. Cameras with autofocus (Camera Module 3) work with a fixed focus; autofocus
   controls (`AfMode`, `LensPosition`) would be new settings in `Config`,
   `controls.go` and the schema.
6. Run the spike (`all`) and compare with [SPIKE.md](SPIKE.md): one frame per
   encoder callback, timestamps, bitrate accuracy (0.9), stream sizes (0.6),
   MJPEG load (0.10).
7. Update the UI popups and HARDWARE.md tables that quote `imx477.json`.

To use another board: VC4 boards need nothing beyond memory tuning; on a Pi 5
re-check bitrate, CPU load and the MJPEG fallback with the spike (§3). For
non-Raspberry-Pi hardware, write a helper that implements
[HELPER_API.md](HELPER_API.md).
