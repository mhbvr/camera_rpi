# Hardware spike: measurements on the Pi 4 + HQ camera

Before relying on assumptions about the camera stack, `spike/m0_spike.py`
measures them on the real hardware. This page keeps the raw output of every
run, with an explanation of what each experiment checks, how, and what the
numbers mean for camweb. Background: [HARDWARE.md](HARDWARE.md),
[ARCHITECTURE.md](ARCHITECTURE.md).

## How to run

Stop camweb first (the spike needs the camera to itself), then on the Pi:

```bash
python3 spike/m0_spike.py all
```

or selected experiments (`info`, `encode`, `sizes`, `controls`, `socket`,
`limits`, `mjpeg`). It prints Markdown (paste it below) and writes test images
to `./spike_out/`. `make deploy PI=pi@raspberrypi.local` copies the repository
to the Pi. A run of `all` takes about two minutes.

Conventions in the results:
- **p50 / p95 / max**: median, 95th percentile and maximum over `n` samples.
- **CPU %**: CPU time of the spike process (Picamera2 and its threads,
  including the encoder output thread) as a percentage of **one** core; the
  Pi 4 has four.
- **Sensor timestamp**: libcamera's `SensorTimestamp`, the time the sensor
  started reading out the frame (CLOCK_MONOTONIC, ns).
- Scene: indoors, dark (auto exposure at 33 ms × gain 16), unless noted.

## Summary

| # | Question | Answer | Consequence in camweb |
|---|---|---|---|
| 0.1 | Which sensor modes and controls exist? | 2028×1520 12-bit up to 45.19 fps; `ExposureTimeMode`/`AnalogueGainMode` present | one mode at ≤ 40 fps; manual exposure via the mode controls |
| 0.2 | Does the encoder hand over whole frames with SPS on keyframes? | yes, 341/341 | one frame per message, no H.264 parser in the helper |
| 0.2 | What is Picamera2's encoder timestamp? | relative to the first frame | the helper adds `encoder.firsttimestamp` |
| 0.3 | Camera-side latency | 35 ms to request complete, 53 ms to encoder output (p50) | latency budget, ARCHITECTURE.md §7 |
| 0.4 | Can we snapshot while encoding? | yes, 0 dropped frames | Capture never stops the stream |
| 0.5 | Does JPEG encoding in Python disturb the video? | callback p95 35 → 47 ms, no drops | encode in a thread (no subprocess) |
| 0.6 | Are the stream sizes applied as requested? | yes, full sensor crop | the sizes in `config.go` |
| 0.7 | How fast do controls apply? | manual exposure after 10 frames | live controls; fewer buffers = faster |
| 0.8 | Unix socket cost for a frame | 0.37 ms for 200 KB | sockets, no shared memory |
| 0.9 | Is the bitrate accurate? | no: delivered = requested × fps / 30; compensation fixes it | helper hands the encoder `bitrate × 30 / fps` |
| 0.10 | Can MJPEG run next to H.264? | yes, no H.264 drops, +5 % CPU; compensated bitrate within ±10 % | MJPEG fallback |

Open follow-ups: visual aspect check of the 0.6 images; glass-to-glass
latency through camweb.

---

# First run (2026-11-06 16:14)

## 0.1 Camera info

**What:** dumps what Picamera2 reports: sensor modes (size, bit depth, max
fps, crop, exposure limits), control ranges (min, max, default), enum names,
colour space and the stream configuration after alignment.
**Why:** the server's constants (sensor mode, sizes, limits) and the
libcamera enum values it sends come from here.
**Reading:**
- Every mode exists in 8-, 10- and 12-bit variants; fewer bits = higher fps.
  camweb uses 2028×1520 12-bit (2×2 binned, full field of view, 45.19 fps).
- `camera_controls` describes the **last configured mode** (here the
  `FrameDurationLimits` minimum 57502 µs belongs to the full-res 8-bit mode),
  so mode-dependent ranges are not used; only the `ExposureTime` and
  `AnalogueGain` maxima are (they match across modes).
- `ScalerCropMaximum (0,0,0,0)` in properties is normal before configuration.
- `picamera2 version: ?` means the version could not be read; not a problem.

- picamera2 version: `?`
- libcamera version: `?`
- properties: `{'Model': 'imx477', 'UnitCellSize': (1550, 1550), 'ScalerCropMaximum': (0, 0, 0, 0)}`

| size | bit depth | fps | crop limits | exposure limits |
|---|---|---|---|---|
| (1332, 990) | 10 | 120.5 | (704, 544, 2664, 1980) | (31, 667234896, 20000) |
| (2028, 1080) | 10 | 74.74 | (8, 456, 4056, 2160) | (47, 674181621, 20000) |
| (2028, 1520) | 10 | 53.77 | (8, 16, 4056, 3040) | (47, 674181621, 20000) |
| (4056, 2160) | 10 | 19.58 | (8, 456, 4056, 2160) | (92, 694422939, 20000) |
| (4056, 3040) | 10 | 14.0 | (8, 16, 4056, 3040) | (92, 694422939, 20000) |
| (1332, 990) | 12 | 101.68 | (704, 544, 2664, 1980) | (37, 667234896, 20000) |
| (2028, 1080) | 12 | 62.81 | (8, 456, 4056, 2160) | (56, 674181621, 20000) |
| (2028, 1520) | 12 | 45.19 | (8, 16, 4056, 3040) | (56, 674181621, 20000) |
| (4056, 2160) | 12 | 16.39 | (8, 456, 4056, 2160) | (110, 694422939, 20000) |
| (4056, 3040) | 12 | 11.72 | (8, 16, 4056, 3040) | (110, 694422939, 20000) |
| (1332, 990) | 8 | 147.91 | (704, 544, 2664, 1980) | (26, 667234896, 20000) |
| (2028, 1080) | 8 | 92.27 | (8, 456, 4056, 2160) | (38, 674181621, 20000) |
| (2028, 1520) | 8 | 66.38 | (8, 16, 4056, 3040) | (38, 674181621, 20000) |
| (4056, 2160) | 8 | 24.32 | (8, 456, 4056, 2160) | (74, 694422939, 20000) |
| (4056, 3040) | 8 | 17.39 | (8, 16, 4056, 3040) | (74, 694422939, 20000) |

Controls (min, max, default):

- `AeConstraintMode`: (0, 3, 0)
- `AeEnable`: (False, True, True)
- `AeExposureMode`: (0, 3, 0)
- `AeFlickerMode`: (0, 1, 0)
- `AeFlickerPeriod`: (100, 1000000, None)
- `AeMeteringMode`: (0, 3, 0)
- `AnalogueGain`: (1.0, 22.2608699798584, 1.0)
- `AnalogueGainMode`: (0, 1, 0)
- `AwbEnable`: (False, True, None)
- `AwbMode`: (0, 7, 0)
- `Brightness`: (-1.0, 1.0, 0.0)
- `CnnEnableInputTensor`: (False, True, False)
- `ColourCorrectionMatrix`: (0.0, 8.0, None)
- `ColourGains`: (0.0, 32.0, None)
- `ColourTemperature`: (100, 100000, None)
- `Contrast`: (0.0, 32.0, 1.0)
- `ExposureTime`: (74, 694422939, 20000)
- `ExposureTimeMode`: (0, 1, 0)
- `ExposureValue`: (-8.0, 8.0, 0.0)
- `FrameDurationLimits`: (57502, 694434742, (33333, 33333))
- `HdrMode`: (0, 4, 0)
- `NoiseReductionMode`: (0, 4, 0)
- `Saturation`: (0.0, 32.0, 1.0)
- `ScalerCrop`: ((8, 16, 64, 64), (8, 16, 4056, 3040), (10, 16, 4053, 3040))
- `Sharpness`: (0.0, 16.0, 1.0)
- `StatsOutputEnable`: (False, True, False)
- `SyncFrames`: (100, 100000, 1000)
- `SyncMode`: (0, 2, 0)

Enum names:

- `AeMeteringMode`: {'CentreWeighted': 0, 'Spot': 1, 'Matrix': 2, 'Custom': 3}
- `AeExposureMode`: {'Normal': 0, 'Short': 1, 'Long': 2, 'Custom': 3}
- `AwbMode`: {'Auto': 0, 'Incandescent': 1, 'Tungsten': 2, 'Fluorescent': 3, 'Indoor': 4, 'Daylight': 5, 'Cloudy': 6, 'Custom': 7}
- `NoiseReductionMode`: {'Off': 0, 'Fast': 1, 'HighQuality': 2, 'Minimal': 3, 'ZSL': 4}
- `ExposureTimeMode`: {'Auto': 0, 'Manual': 1}
- `AnalogueGainMode`: {'Auto': 0, 'Manual': 1}

- video config colour space: `<libcamera.ColorSpace 'Rec709'>`
- applied main: `{'format': 'YUV420', 'size': (2028, 1520), 'preserve_ar': True, 'stride': 2048, 'framesize': 4669440}`, lores: `{'format': 'YUV420', 'size': (1440, 1080), 'preserve_ar': False, 'stride': 1472, 'framesize': 2384640}`, sensor: `{'bit_depth': 12, 'output_size': (2028, 1520)}`

## 0.2-0.5 Encoding, latency, snapshots while encoding

**What:** streams H.264 at 1440×1080@30 for 8 s through a custom Picamera2
`Output` that records every encoder callback, then takes 10 snapshots from the
main stream while encoding continues.
- **0.2** Does each `outputframe` call carry exactly one frame (one picture)?
  Do keyframes carry SPS/PPS (`repeat=True`)? What is the `timestamp`
  argument?
- **0.3** Latency from the sensor timestamp to the metadata callback (frame
  processed by the ISP) and to the encoder output.
- **0.4** Snapshot cost: `capture_request()` + copy of the 2028×1520 YUV
  buffer, and whether H.264 frames are lost meanwhile (timestamp gaps
  > 1.5 frame intervals).
- **0.5** JPEG encode time in a Python thread, and how much it delays the
  encoder callbacks (the GIL is shared).

**Reading:**
- "callbacks with != 1 picture: 0" and "keyframes with SPS 12/12": the helper
  can send each callback as one complete frame, with no parsing and no extra
  frame of delay.
- `timestamp` starts at 0 for the first frame; adding
  `encoder.firsttimestamp` gives the sensor time. The resulting 53 ms p50
  latency (vs. 35 ms to the metadata callback) means the hardware encode
  takes about 18 ms; the tail (p95 73 ms) is probably the large keyframes.
- "callback interval during snapshots p95 46.7 ms" (baseline 35 ms): the JPEG
  encode delays frames by up to ~12 ms but loses none (0 gaps). Accepted.
- The helper uses 24 % of one core at 30 fps.

- outputframe extra args: 2 positional, kwargs []
- callbacks: 341; callbacks with != 1 picture (should be 0): **0**
- keyframes: 12; keyframes with SPS: 12; non-key with SPS: 0
- first callback keyframe: True
- `timestamp` argument sample: [0, 33324, 66649] (type int)
- SensorTimestamp sample: [1299866782000, 1299900106000, 1299933431000]
- timestamp*1000 == SensorTimestamp for 0 of 341 frames
- sensor timestamp -> outputframe (if monotonic clock): n/a
- sensor timestamp -> outputframe via encoder.firsttimestamp: mean 60.29 ms, p50 52.78, p95 73.43, max 85.09 (n=341)
- sensor timestamp -> post_callback: mean 38.93 ms, p50 35.27, p95 44.73, max 49.19 (n=341)
- callback interval, baseline: mean 33.41 ms, p50 33.32, p95 34.95, max 51.98 (n=216)
- callback interval during snapshots (GIL impact): mean 33.12 ms, p50 33.22, p95 46.73, max 48.34 (n=93)
- timestamp gaps > 1.5 frame (dropped frames): **0**
- snapshot copy (capture_request+copy): mean 13.37 ms, p50 10.99, p95 30.75, max 30.75 (n=10)
- snapshot JPEG encode 2028x1520: mean 93.96 ms, p50 89.13, p95 144.85, max 144.85 (n=10)
- process CPU while encoding: 24% of one core

## 0.6 Stream sizes in the 2028x1520 mode

**What:** configures each offered stream size as lores next to the 2028×1520
main stream and saves one frame of each as JPEG.
**Why:** lores must have the same aspect ratio as main, and libcamera may
align sizes; the applied sizes become constants in Go.
**Reading:** all sizes are applied unchanged (the stride is padded to a
multiple of 32 or 64 bytes, which the helper handles), and the crop is the full
sensor. The images still need a visual check (a round object must stay round).

| requested lores | applied lores | stride | main | ScalerCrop | files |
|---|---|---|---|---|---|
| (1440, 1080) | (1440, 1080) | 1472 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_1440x1080.jpg, spike_out/main_1440x1080.jpg |
| (1024, 768) | (1024, 768) | 1024 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_1024x768.jpg, spike_out/main_1024x768.jpg |
| (640, 480) | (640, 480) | 640 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_640x480.jpg, spike_out/main_640x480.jpg |

Check the images: a circle must stay round in both lores and main.

## 0.7 Controls

**What:** reads auto-exposure values, switches to manual `ExposureTime=5000`,
counts frames until the metadata shows it, returns to auto, and changes
`FrameDurationLimits` while running.
**Reading:**
- Auto exposure sits at 33 ms × gain 16: the 30 fps frame time and the
  profile's gain limit, i.e. the scene was dark (see HARDWARE.md §8).
- A control change shows up **10 frames** later (~330 ms at 30 fps): the
  libcamera pipeline depth with 6 buffers. Fewer buffers should shorten it.
- Returning to auto works through `ExposureTimeMode`/`AnalogueGainMode`, and
  the frame rate can change without a restart (50 ms → 49992 µs frames).

- auto exposure: ExposureTime=33013 AnalogueGain=16.0
- manual ExposureTime=5000 applied after **10** frames
- back to auto via ExposureTimeMode/AnalogueGainMode = 0: ExposureTime=33013 AnalogueGain=15.753846168518066 (should differ from 5000/2.0 unless the scene matches)
- FrameDurationLimits -> 50000 at runtime: last FrameDuration values [49992, 49992, 49992, 49992, 49992]

## 0.8 Unix socket hand-off

**What:** sends 300 messages of 200 KB (a large H.264 keyframe) over a Unix
stream socket from Python to a reader thread and times each until fully
received.
**Reading:** 0.37 ms per keyframe-sized message, about 1 % of the 33 ms frame
interval. Shared memory would save nothing noticeable (ARCHITECTURE.md §3.2).

- 200 KB messages, sender -> reader complete: mean 0.37 ms, p50 0.36, p95 0.43, max 0.96 (n=300)

## 0.9 Encoder limits (1440x1080)

**What:** encodes 1440×1080 at 30 and 40 fps at several bitrates, with and
without Picamera2's `framerate=` argument, and measures delivered frame rate,
bitrate and CPU over 6 s.
**Why:** to check that the hardware encoder sustains 40 fps and delivers the
requested bitrate.
**Reading:** 40 fps is fine, but the bitrate is not: at 40 fps the encoder
delivers 8.03 Mbit/s for 6 requested (2 → 2.69, 12 → 16.0), with or without
`framerate=`. Every row fits `delivered = requested × fps / 30`: the encoder's
rate control assumes 30 fps. See the analysis and the re-run below.

Bitrate accuracy with and without `framerate=` passed to H264Encoder.

| fps | framerate arg | bitrate | delivered fps | Mbit/s | CPU % |
|---|---|---|---|---|---|
| 30 | no | 6 M | 30.0 | 6.03 | 33 |
| 30 | yes | 6 M | 29.8 | 6.03 | 21 |
| 40 | no | 6 M | 40.0 | 8.04 | 43 |
| 40 | yes | 6 M | 40.0 | 8.03 | 42 |
| 40 | yes | 2 M | 39.8 | 2.69 | 28 |
| 40 | yes | 12 M | 39.8 | 16.00 | 28 |


---

## Analysis of the first run (2026-11-06 16:14)

Measured on a Raspberry Pi 4 with the HQ camera. The first run (16:02) gave
the same numbers for 0.1–0.8; this run adds two measurements (marked new).

| # | Question | Result | Verdict / action |
|---|---|---|---|
| 0.1 | Sensor modes | 8/10/12-bit variants of every mode. 2028×1520 12-bit up to **45.19 fps** | OK: the 40 fps limit is safe |
| 0.1 | Exposure controls | `ExposureTimeMode` / `AnalogueGainMode` present | OK: the server uses them |
| 0.1 | Control ranges | `camera_controls` reflects the last configured mode (`FrameDurationLimits` min 57502 µs is the full-res 8-bit mode) | only ExposureTime/AnalogueGain maxima are used, and they match across modes |
| 0.1 | Colour space | Rec709 (limited range) | the snapshot expands the range; a small 709 vs 601 hue difference is accepted |
| 0.2 | One frame per encoder callback | 341/341 hold exactly one picture; every keyframe carries SPS; the first callback is a keyframe | OK |
| 0.2 | `timestamp` argument | relative to the first frame; `encoder.firsttimestamp + timestamp` gives the sensor time in µs (CLOCK_MONOTONIC) | OK: confirmed by the plausible latency below; the helper uses it |
| 0.3 | Capture → request complete | sensor timestamp → post_callback **35 ms p50**, 39 ms mean (33 ms exposure, dark scene) | as expected |
| 0.3 | **(new)** Capture → encoder output | sensor timestamp → `outputframe` **53 ms p50**, 60 ms mean, 73 ms p95, 85 ms max | so the hardware encode takes ~18 ms p50; the tail is probably the large I-frames |
| 0.4 | Snapshot while encoding | **0 dropped frames**; copy 13 ms mean | OK |
| 0.5 | JPEG encode, GIL impact | encode 94 ms mean; callback interval p95 35 → 47 ms during snapshots, max unchanged | OK: encoding stays in a thread |
| 0.5 | Helper CPU | 24 % of one core at 1440×1080@30 | OK |
| 0.6 | Stream sizes | applied unchanged; full-sensor ScalerCrop | **visual check of `spike_out/*.jpg` still needed** |
| 0.7 | Manual exposure latency | 10 frames | libcamera pipeline depth; try *Camera buffers* = 4 |
| 0.7 | Back to auto / runtime frame rate | work | OK |
| 0.8 | Unix socket, 200 KB | 0.37 ms | OK |
| 0.9 | **(new)** Bitrate with `framerate=` | **no effect**: 40 fps, 6 Mbit/s requested → 8.03 delivered with and without it. Exact fit to `delivered = requested × fps / 30` (6.03 at 30 fps; 2.69 / 8.03 / 16.0 at 40 fps) | **fixed differently:** the helper now gives the encoder `bitrate × 30 / fps` (capped at 25 Mbit/s). Frame-rate changes still restart the encoder, because the compensation depends on fps |

### Camera-side latency budget (dark scene, 33 ms exposure)
Sensor timestamp → request complete 35 ms → encoder output 53 ms (p50) →
socket < 1 ms. Network, decode and display come on top of that. Glass-to-glass
on the LAN should be roughly 70–100 ms. Shorter exposures in normal light
reduce the first stage.

### Follow-ups
- Re-run `python3 spike/m0_spike.py limits` to confirm the compensation
  (rows marked "compensated: yes" should deliver the requested bitrate,
  including at 15 fps).
- Check `spike_out/*.jpg` for aspect ratio.
- Measure glass-to-glass latency with camweb.

---

## 0.9 Re-run: bitrate compensation (2026-11-06 18:57)

**What:** after the helper started handing the encoder `bitrate × 30 / fps`,
the `limits` experiment compares uncompensated and compensated rows at 15, 30
and 40 fps. "compensated: yes" rows should deliver the requested bitrate.

| fps | compensated | requested | delivered fps | Mbit/s | CPU % |
|---|---|---|---|---|---|
| 30 | no | 6 M | 29.8 | 6.15 | 22 |
| 40 | no | 6 M | 39.8 | 7.99 | 42 |
| 40 | yes | 6 M | 40.2 | 6.04 | 43 |
| 40 | yes | 2 M | 40.0 | 2.00 | 43 |
| 40 | yes | 12 M | 40.0 | 12.02 | 43 |
| 15 | no | 6 M | 15.0 | 3.07 | 11 |
| 15 | yes | 6 M | 15.0 | 6.29 | 11 |

**Verdict: the compensation works.** Compensated rows deliver the requested
bitrate at 40 fps (within 1 %) and at 15 fps (+5 %). That deviation is
comparable to the uncompensated 30 fps row (+2.5 %), i.e. normal rate-control
and measurement spread; about six keyframes fall in the 6 s window.
Uncompensated rows again match requested × fps / 30. CPU depends on frame rate,
not bitrate (≈ 11 / 22 / 43 % at 15 / 30 / 40 fps). 0.9 is closed.

Remaining follow-ups: aspect check of `spike_out/*.jpg`; glass-to-glass
latency with camweb.

---

## 0.10 MJPEG fallback in parallel with H.264 (2026-11-06 21:54)

**What:** runs H.264 at 1440×1080@30 (6 Mbit/s) alone, then together with a
second, hardware `MJPEGEncoder` on the same lores stream, for several MJPEG
bitrates and frame-skip values (`frame_skip_count`: every n-th frame). For 6 s
each it measures H.264 frame rate and dropped frames (timestamp gaps), MJPEG
frame rate, bitrate and frame size, and CPU.
**Why:** the MJPEG fallback (ARCHITECTURE.md §5.3) must not hurt the main
H.264 stream, and its bitrate setting should mean what it says.

H.264 1440x1080@30 at 6 Mbit/s, plus a hardware `MJPEGEncoder` on the same
lores stream.

| MJPEG | H.264 fps | H.264 drops | MJPEG fps | MJPEG Mbit/s | MJPEG KB/frame | CPU % |
|---|---|---|---|---|---|---|
| none (H.264 only) | 29.8 | 0 | – | – | – | 22 |
| 4 M, every 2. | 29.8 | 0 | 14.8 | 3.00 | 25 | 27 |
| 8 M, every 2. | 29.8 | 0 | 14.8 | 4.12 | 35 | 26 |
| 16 M, every 2. | 29.8 | 0 | 14.8 | 8.40 | 71 | 27 |
| 8 M, every 1. | 29.8 | 0 | 29.8 | 8.12 | 34 | 28 |
| 16 M, every 1. | 29.8 | 0 | 29.8 | 16.65 | 70 | 28 |

**Verdict:**
- **H.264 is unaffected:** 29.8 fps and 0 dropped frames in every case.
- **Pi load is small:** about +5 % CPU (encoding is done in hardware). The
  Pi 4 runs both encoders comfortably.
- **`frame_skip_count` works:** every 2nd frame gives exactly half the frame
  rate.
- **Bitrate follows the H.264 quirk:** the MJPEG encoder budgets bits per
  frame assuming 30 fps. With every frame, delivered ≈ requested; with every
  2nd frame, about half (8 → 4.1, 16 → 8.4). 4 M every 2nd delivered 3.0
  instead of 2: a quality floor at small sizes.
- **Fixed:** the helper now hands the encoder `bitrate × 30 / MJPEG fps`
  (capped at 25 Mbit/s), so `mjpeg.bitrate` is the delivered bitrate. The
  setting is re-sent after a frame-rate change.
- **Follow-up:** re-run `python3 spike/m0_spike.py mjpeg`. Rows marked
  "compensated" should deliver the requested bitrate.

## 0.10 Re-run: MJPEG bitrate compensation (2026-11-06 22:02)

**What:** the same experiment after the helper started compensating MJPEG for
its frame rate (`bitrate × 30 / (fps / skip)`); the row without
"compensated" is the old behaviour, for comparison.

Same setup. Rows marked "compensated" hand the encoder
`bitrate × 30 / MJPEG fps`, as the helper now does.

| MJPEG | H.264 fps | H.264 drops | MJPEG fps | MJPEG Mbit/s | MJPEG KB/frame | CPU % |
|---|---|---|---|---|---|---|
| none (H.264 only) | 29.8 | 0 | – | – | – | 22 |
| 8 M, every 2. | 29.8 | 0 | 14.8 | 4.16 | 35 | 27 |
| 4 M, every 2., compensated | 29.8 | 0 | 14.8 | 4.16 | 35 | 27 |
| 8 M, every 2., compensated | 29.8 | 0 | 14.8 | 8.76 | 74 | 27 |
| 8 M, every 1., compensated | 29.8 | 0 | 29.8 | 8.39 | 35 | 28 |
| 4 M, every 3., compensated | 29.8 | 0 | 10.0 | 3.64 | 45 | 25 |

**Verdict:**
- **Compensation works:** delivered bitrate is within about ±10 % of the
  request (4 → 4.16, +4 %; 8 → 8.76, +10 %; 8 → 8.39, +5 %; 4 → 3.64, −9 %),
  against −48 % uncompensated (8 M every 2nd → 4.16).
- **H.264 is still unaffected:** 29.8 fps, 0 drops, at most +6 % CPU.
- Spike 0.10 is closed.
