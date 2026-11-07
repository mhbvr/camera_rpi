# Hardware spike: measurements on the Pi 4 + HQ camera

`spike/m0_spike.py` measures the camera stack on the real hardware: what the
camera reports, encoder behaviour, latency, snapshots, controls, IPC and the
two encoders running together. This page holds the latest run, which matches
the current code, with an explanation of what each experiment checks and what
the numbers mean for camweb. Background: [HARDWARE.md](HARDWARE.md),
[ARCHITECTURE.md](ARCHITECTURE.md).

**Run:** 2026-11-07 01:01, Raspberry Pi 4, HQ camera, indoors, dark scene
(auto exposure at 33 ms × gain 16).
In 0.9 and 0.10 that run labelled the helper's method `s_parm` and also had
rows for an earlier bitrate workaround, which the code no longer uses; those
rows are left out here.

## How to run

Stop camweb first (the spike needs the camera to itself), then on the Pi:

```bash
python3 spike/m0_spike.py all
```

or selected experiments (`info`, `encode`, `sizes`, `controls`, `socket`,
`limits`, `mjpeg`). It prints Markdown and writes test images to
`./spike_out/`. `make deploy PI=pi@raspberrypi.local` copies the binary and
the script to the Pi. `all` takes about two minutes.

Conventions:
- **p50 / p95 / max**: median, 95th percentile and maximum over `n` samples.
- **CPU %**: CPU time of the spike process (Picamera2 and its threads) as a
  percentage of **one** core; the Pi 4 has four.
- **Sensor timestamp**: libcamera's `SensorTimestamp`, when the sensor started
  reading out the frame (CLOCK_MONOTONIC).
- **helper** rows in 0.9 and 0.10 create the encoders exactly as camweb does
  (`make_h264_encoder`, `make_mjpeg_encoder` in `camhelper.py`: the real frame
  rate is passed to the encoder). **no frame rate** rows show the driver's
  default for comparison.

## Summary

| # | Question | Answer | Consequence in camweb |
|---|---|---|---|
| 0.1 | Which sensor modes and controls exist? | 2028×1520 12-bit up to 45.19 fps; `ExposureTimeMode`/`AnalogueGainMode` present | one mode at ≤ 40 fps; manual exposure via the mode controls |
| 0.2 | Does the encoder hand over whole frames with SPS on keyframes? | yes, 350/350 | one frame per message, no H.264 parser in the helper |
| 0.2 | What is Picamera2's encoder timestamp? | relative to the first frame | the helper adds `encoder.firsttimestamp` |
| 0.3 | Camera-side latency | 35 ms to request complete, 52 ms to encoder output (p50) | latency budget, ARCHITECTURE.md §7 |
| 0.4 | Can we snapshot while encoding? | yes, 0 dropped frames | Capture never stops the stream |
| 0.5 | Does JPEG encoding in Python disturb the video? | callback p95 34.5 → 43 ms, no drops | encode in a thread |
| 0.6 | Are the stream sizes applied as requested? | yes, full sensor crop | the sizes in `config.go` |
| 0.7 | How fast do controls apply? | manual exposure after 9 frames | live controls |
| 0.8 | Unix socket cost for a frame | 0.34 ms for 200 KB | sockets, no shared memory |
| 0.9 | Is the H.264 bitrate accurate? | yes, when the encoder gets the frame rate (40 fps: 6 → 6.04); without it the driver assumes 30 fps | helper passes the frame rate |
| 0.10 | Can MJPEG run next to H.264? | yes: no H.264 drops, about +5 % CPU, MJPEG bitrate within ±10 % | MJPEG fallback |

Open follow-ups: visual aspect check of the 0.6 images; glass-to-glass
latency through camweb.

Checked separately (2026-11-07): the H.264 SPS states the frame rate set in
camweb (8 fps → `time_scale = 16`, `num_units_in_tick = 1`, i.e. 8 fps;
HARDWARE.md §3.1).

---

## 0.1 Camera info

**What:** dumps what Picamera2 reports: sensor modes (size, bit depth, max
fps, crop, exposure limits), control ranges (min, max, default), enum names,
colour space and the stream configuration after alignment.
**Why:** the server's constants (sensor mode, sizes, limits) and the libcamera
enum values it sends come from here.
**Reading:**
- Every mode exists in 8-, 10- and 12-bit variants; fewer bits = higher fps.
  camweb uses 2028×1520 12-bit (2×2 binned, full field of view, 45.19 fps).
- `camera_controls` describes the **last configured mode** (here the
  `FrameDurationLimits` minimum 57502 µs belongs to the full-res 8-bit mode),
  so mode-dependent ranges are not used; only the `ExposureTime` and
  `AnalogueGain` maxima are (they match across modes).
- Colour space Rec709 (limited range): snapshots expand it to full range.
- `ScalerCropMaximum (0,0,0,0)` before configuration and `picamera2 version:
  ?` (version not readable) are harmless.

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

## 0.2–0.5 Encoding, latency, snapshots while encoding

**What:** streams H.264 at 1440×1080@30 for 8 s through a custom Picamera2
`Output` that records every encoder callback, then takes 10 snapshots from the
main stream while encoding continues.
- **0.2** Does each `outputframe` call carry exactly one frame? Do keyframes
  carry SPS/PPS (`repeat=True`)? What is the `timestamp` argument?
- **0.3** Latency from the sensor timestamp to the metadata callback (frame
  processed by the ISP) and to the encoder output.
- **0.4** Snapshot cost (`capture_request()` + copy of the 2028×1520 YUV
  buffer) and whether H.264 frames are lost meanwhile (timestamp gaps
  > 1.5 frame intervals).
- **0.5** JPEG encode time in a Python thread, and how much it delays the
  encoder callbacks (the GIL is shared).

**Reading:**
- 0 callbacks with ≠ 1 picture and SPS on 12/12 keyframes: the helper sends
  each callback as one complete frame, with no parsing and no extra frame of
  delay.
- `timestamp` starts at 0; adding `encoder.firsttimestamp` gives the sensor
  time. Sensor → metadata callback 35 ms p50, → encoder output 52 ms p50
  (62 ms p95): the hardware encode takes about 17 ms; the tail is the large
  keyframes.
- During snapshots the callback interval p95 rises from 34.5 to 43 ms (JPEG
  encoding holds the GIL), but no frame is lost. Accepted.
- Snapshot: copy 15 ms, JPEG encode 125 ms in a worker thread (depends on
  scene detail). Helper CPU 16 % of one core at 30 fps.

- outputframe extra args: 2 positional, kwargs []
- callbacks: 350; callbacks with != 1 picture (should be 0): **0**
- keyframes: 12; keyframes with SPS: 12; non-key with SPS: 0
- first callback keyframe: True
- `timestamp` argument sample: [0, 33324, 66650] (type int)
- SensorTimestamp sample: [35898315000, 35931639000, 35964965000]
- timestamp*1000 == SensorTimestamp for 0 of 350 frames
- sensor timestamp -> outputframe (if monotonic clock): n/a
- sensor timestamp -> outputframe via encoder.firsttimestamp: mean 52.95 ms, p50 51.73, p95 62.02, max 67.99 (n=350)
- sensor timestamp -> post_callback: mean 35.81 ms, p50 35.06, p95 42.85, max 47.12 (n=351)
- callback interval, baseline: mean 33.32 ms, p50 33.32, p95 34.48, max 39.60 (n=217)
- callback interval during snapshots (GIL impact): mean 33.32 ms, p50 33.31, p95 43.13, max 46.75 (n=102)
- timestamp gaps > 1.5 frame (dropped frames): **0**
- snapshot copy (capture_request+copy): mean 14.87 ms, p50 13.53, p95 21.62, max 21.62 (n=10)
- snapshot JPEG encode 2028x1520: mean 124.88 ms, p50 119.96, p95 176.54, max 176.54 (n=10)
- process CPU while encoding: 16% of one core

## 0.6 Stream sizes in the 2028×1520 mode

**What:** configures each offered stream size as lores next to the 2028×1520
main stream and saves one frame of each as JPEG.
**Why:** lores must have the same aspect ratio as main, and libcamera may
align sizes; the applied sizes are constants in Go.
**Reading:** all sizes are applied unchanged (the stride is padded, which the
helper handles), with the full sensor crop. The saved images still need a
visual check: a round object must stay round.

| requested lores | applied lores | stride | main | ScalerCrop | files |
|---|---|---|---|---|---|
| (1440, 1080) | (1440, 1080) | 1472 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_1440x1080.jpg, spike_out/main_1440x1080.jpg |
| (1024, 768) | (1024, 768) | 1024 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_1024x768.jpg, spike_out/main_1024x768.jpg |
| (640, 480) | (640, 480) | 640 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_640x480.jpg, spike_out/main_640x480.jpg |

## 0.7 Controls

**What:** reads auto-exposure values, switches to manual `ExposureTime=5000`,
counts frames until the metadata shows it, returns to auto, and changes
`FrameDurationLimits` while running.
**Reading:**
- Auto exposure at 33 ms × gain 16: the 30 fps frame time and the profile's
  gain limit, i.e. a dark scene (HARDWARE.md §8).
- A control change shows up **9 frames** later (~300 ms at 30 fps): the
  libcamera pipeline depth with 6 buffers.
- Returning to auto works through `ExposureTimeMode`/`AnalogueGainMode`, and
  the frame rate changes without a restart (50 ms → 49992 µs frames).

- auto exposure: ExposureTime=33013 AnalogueGain=16.0
- manual ExposureTime=5000 applied after **9** frames
- back to auto via ExposureTimeMode/AnalogueGainMode = 0: ExposureTime=33013 AnalogueGain=15.753846168518066 (should differ from 5000/2.0 unless the scene matches)
- FrameDurationLimits -> 50000 at runtime: last FrameDuration values [49992, 49992, 49992, 49992, 49992]

## 0.8 Unix socket hand-off

**What:** sends 300 messages of 200 KB (a large H.264 keyframe) over a Unix
stream socket from Python to a reader thread and times each until fully
received.
**Reading:** 0.34 ms per keyframe-sized message, about 1 % of the 33 ms frame
interval. Shared memory would save nothing noticeable (ARCHITECTURE.md §3.2).

- 200 KB messages, sender -> reader complete: mean 0.34 ms, p50 0.36, p95 0.40, max 0.61 (n=300)

## 0.9 Encoder limits and bitrate (1440×1080)

**What:** encodes for 6 s at several frame rates and bitrates and measures
delivered frame rate, bitrate and CPU, with the helper's encoder setup and
without a frame rate.
**Why:** the Pi's encoders plan bits per frame for 30 fps unless they are told
the frame rate (HARDWARE.md §3.1). The helper passes it; this checks that the
requested bitrate is then delivered.
**Reading:**
- **helper:** delivered = requested (40 fps: 6 → 6.04, 12 → 12.06; 15 fps:
  6 → 6.42, about +7 %, as keyframes are a larger share at low frame rates).
- **no frame rate:** delivered = requested × fps / 30 (40 fps: 8.01; 15 fps:
  3.14). At 30 fps both are the same.
- 40 fps is sustained. CPU depends on the frame rate, not the bitrate (about
  11 % at 15 fps, 42 % at 40 fps).

| fps | method | requested | delivered fps | Mbit/s | CPU % |
|---|---|---|---|---|---|
| 30 | no frame rate (= helper at 30 fps) | 6 M | 30.0 | 6.04 | 32 |
| 40 | helper | 6 M | 40.0 | 6.04 | 42 |
| 40 | helper | 12 M | 40.0 | 12.06 | 42 |
| 15 | helper | 6 M | 15.0 | 6.42 | 11 |
| 40 | no frame rate | 6 M | 39.8 | 8.01 | 27 |
| 15 | no frame rate | 6 M | 15.0 | 3.14 | 11 |

## 0.10 MJPEG fallback in parallel with H.264

**What:** H.264 at 1440×1080@30 (6 Mbit/s) alone, then together with the
hardware `MJPEGEncoder` on the same lores stream, for several MJPEG bitrates
and frame-skip values (`frame_skip_count`: every n-th frame). For 6 s each:
H.264 frame rate and dropped frames, MJPEG frame rate, bitrate, frame size,
CPU.
**Why:** the MJPEG fallback (ARCHITECTURE.md §5.3) must not hurt the H.264
stream, and `mjpeg.bitrate` should be the delivered bitrate.
**Reading:**
- **H.264 is unaffected:** 29.8 fps and 0 dropped frames in every case.
- **Small Pi load:** about +5 % CPU (21 % → 25–28 %); both encoders are
  hardware.
- **Bitrate with the helper's setup** (MJPEG frame rate fps / skip passed to
  the encoder): within about ±10 % of the request (8 → 8.31 every frame,
  8 → 7.31 every 2nd, 4 → 3.78 every 3rd). Without it, the encoder plans for
  30 fps and every 2nd frame delivers half (8 → 4.13).
- MJPEG frames are 35–62 KB at 1440×1080.

| MJPEG | H.264 fps | H.264 drops | MJPEG fps | MJPEG Mbit/s | MJPEG KB/frame | CPU % |
|---|---|---|---|---|---|---|
| none (H.264 only) | 29.8 | 0 | – | – | – | 21 |
| 8 M, every 1., helper | 29.8 | 0 | 29.8 | 8.31 | 35 | 28 |
| 8 M, every 2., helper | 29.8 | 0 | 14.8 | 7.31 | 62 | 27 |
| 4 M, every 3., helper | 29.8 | 0 | 10.0 | 3.78 | 47 | 25 |
| 8 M, every 2., no frame rate | 29.8 | 0 | 14.8 | 4.13 | 35 | 27 |
