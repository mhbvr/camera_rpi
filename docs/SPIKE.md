# M0 spike results (2026-11-06 16:02)

## 0.1 Camera info

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

- outputframe extra args: 2 positional, kwargs []
- callbacks: 342; callbacks with != 1 picture (should be 0): **0**
- keyframes: 12; keyframes with SPS: 12; non-key with SPS: 0
- first callback keyframe: True
- `timestamp` argument sample: [0, 33324, 66647] (type int)
- SensorTimestamp sample: [587098297000, 587131621000, 587164944000]
- timestamp*1000 == SensorTimestamp for 0 of 342 frames
- sensor timestamp -> outputframe (if monotonic clock): n/a
- sensor timestamp -> post_callback: mean 39.99 ms, p50 43.48, p95 44.96, max 49.48 (n=342)
- callback interval, baseline: mean 33.42 ms, p50 33.31, p95 35.34, max 52.60 (n=217)
- callback interval during snapshots (GIL impact): mean 33.10 ms, p50 33.23, p95 46.17, max 50.84 (n=93)
- timestamp gaps > 1.5 frame (dropped frames): **0**
- snapshot copy (capture_request+copy): mean 14.66 ms, p50 10.64, p95 35.97, max 35.97 (n=10)
- snapshot JPEG encode 2028x1520: mean 95.16 ms, p50 89.33, p95 152.59, max 152.59 (n=10)
- process CPU while encoding: 27% of one core

## 0.6 Stream sizes in the 2028x1520 mode

| requested lores | applied lores | stride | main | ScalerCrop | files |
|---|---|---|---|---|---|
| (1440, 1080) | (1440, 1080) | 1472 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_1440x1080.jpg, spike_out/main_1440x1080.jpg |
| (1024, 768) | (1024, 768) | 1024 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_1024x768.jpg, spike_out/main_1024x768.jpg |
| (640, 480) | (640, 480) | 640 | (2028, 1520) | (8, 16, 4056, 3040) | spike_out/lores_640x480.jpg, spike_out/main_640x480.jpg |

Check the images: a circle must stay round in both lores and main.

## 0.7 Controls

- auto exposure: ExposureTime=33013 AnalogueGain=16.0
- manual ExposureTime=5000 applied after **10** frames
- back to auto via ExposureTimeMode/AnalogueGainMode = 0: ExposureTime=33013 AnalogueGain=15.753846168518066 (should differ from 5000/2.0 unless the scene matches)
- FrameDurationLimits -> 50000 at runtime: last FrameDuration values [49992, 49992, 49992, 49992, 49992]

## 0.8 Unix socket hand-off

- 200 KB messages, sender -> reader complete: mean 0.38 ms, p50 0.37, p95 0.42, max 1.20 (n=300)

## 0.9 Encoder limits (1440x1080@40)

| bitrate | delivered fps | Mbit/s | CPU % |
|---|---|---|---|
| 2 M | 40.0 | 2.68 | 42 |
| 6 M | 39.8 | 7.96 | 28 |
| 12 M | 40.2 | 16.16 | 44 |

---

## Analysis (2026-11-06)

Measured on a Raspberry Pi 4 with the HQ camera. "OK" means the design
assumption holds.

| # | Question | Result | Verdict / action |
|---|---|---|---|
| 0.1 | Sensor modes | This driver offers 8/10/12-bit variants of every mode. 2028×1520 12-bit runs up to **45.19 fps** (10-bit 53.8, 8-bit 66.4); full-res 12-bit 11.7 fps | OK: our mode exists; the 40 fps limit is safe |
| 0.1 | Exposure controls | `ExposureTimeMode` / `AnalogueGainMode` exist (newer libcamera) | the server already uses them when present |
| 0.1 | Control ranges | `camera_controls` reflects the *last configured* mode (`FrameDurationLimits` min 57502 µs = the full-res 8-bit mode, left over from enumerating `sensor_modes`) | only the maxima of ExposureTime/AnalogueGain are used for validation, and those match across modes; documented |
| 0.1 | Colour space | video config is Rec709 (limited range) | the snapshot's range expansion applies; small hue difference from the 709→601 matrix remains (accepted) |
| 0.2 | One frame per encoder callback | 342/342 callbacks hold exactly one picture; every keyframe carries SPS; first callback is a keyframe | OK: no splitter needed |
| 0.2 | `outputframe` signature | 2 extra positional args | OK: handled by `*args` |
| 0.2 | `timestamp` argument | **relative** to the first frame (0, 33324, …), not `SensorTimestamp` | fixed: helper adds `encoder.firsttimestamp`, so `pts_us` is the absolute sensor time |
| 0.3 | Capture latency | sensor timestamp → request completed (post_callback): **40 ms** mean, 43 ms p50, with 33 ms exposure in a dark scene | encoder latency not measured yet: the re-run reports sensor → encoder callback directly |
| 0.4 | Snapshot while encoding | **0 dropped frames**; copy 15 ms mean (36 ms p95) | OK |
| 0.5 | JPEG encode, GIL impact | encode 95 ms mean (153 ms p95). Callback interval p95 35 → 46 ms during snapshots, max unchanged (52 → 51 ms) | acceptable: **keep encoding in a thread** (decision recorded), no subprocess |
| 0.5 | Helper CPU | 27 % of one core at 1440×1080@30 | OK |
| 0.6 | Stream sizes | all three applied unchanged; full-sensor `ScalerCrop` (8,16,4056,3040) | **visual check of `spike_out/*.jpg` still needed** (circle stays round) |
| 0.7 | Manual exposure latency | applied after **10 frames** (~330 ms at 30 fps) | expected libcamera pipeline depth (buffers in flight + sensor delay). Fewer *Camera buffers* should shorten it; try 4 |
| 0.7 | Back to auto | `ExposureTimeMode/AnalogueGainMode = 0` returns to auto (33013 µs / gain 15.8 again) | OK: matches the server's mapping |
| 0.7 | Frame rate at runtime | `FrameDurationLimits` → 50000 gives 49992 µs frames | OK |
| 0.8 | Unix socket, 200 KB | 0.38 ms mean | OK: negligible |
| 0.9 | Encoder at 40 fps | 40 fps delivered at 2–12 Mbit/s. **Bitrate overshoots by 33 %** (2→2.68, 6→7.96, 12→16.2 Mbit/s); 40/30 = 1.33, so the encoder's rate control assumes 30 fps | fixed: the frame rate is passed to `H264Encoder(framerate=…)`, and a frame-rate change now restarts the encoder |

### Follow-ups
- Re-run `python3 spike/m0_spike.py encode limits` to confirm the bitrate fix
  (table now compares with and without `framerate=`) and to get sensor →
  encoder latency.
- Check `spike_out/*.jpg` for aspect ratio.
- Measure glass-to-glass latency with camweb (camera pointed at an on-screen
  millisecond clock). The stats overlay now also shows the camera-side
  latency (sensor timestamp → socket write).
