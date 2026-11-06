# camera_rpi

Prototype web UI for a **Raspberry Pi 4 + HQ Camera (IMX477)**:

- low-latency live H.264 stream in the browser (WebCodecs, no plugins, no JS libraries)
- live camera settings (exposure, gain, white balance, image tuning, frame rate) plus stream size, bitrate and keyframe interval
- **Capture** button: a 2028×1520 JPEG from the camera's main stream, taken without interrupting the video

Architecture, protocol and plan: [`docs/DESIGN.md`](docs/DESIGN.md),
[`docs/PROTOCOL.md`](docs/PROTOCOL.md), [`docs/PLAN.md`](docs/PLAN.md).

```
camhelper.py (Python, Picamera2)  ──ctl.sock: JSON-RPC 2.0──▶  camweb (Go, stdlib only)  ──HTTP──▶  browser
  libcamera, HW H.264 encoder     ──video.sock: framed H.264─▶  hub, config, API              fetch + WebCodecs
```

## Requirements

- Raspberry Pi OS Bookworm (or newer), camera detected (`rpicam-hello --list-cameras`)
- `python3-picamera2` (preinstalled on desktop images; on Lite:
  `sudo apt install -y python3-picamera2 --no-install-recommends`)
- To build: Go 1.22+ (on any machine; the binary cross-compiles)

## Quick start on the Pi

Build on your computer and copy the binary over (the Python helper is embedded in it):

```bash
make build-arm64
```

```bash
scp bin/camweb-arm64 pi@raspberrypi.local:camweb
```

On the Pi:

```bash
./camweb -listen :8080
```

Flags: `-hflip`, `-vflip` (both = rotate 180°), `-data DIR` (config and
captures, default `./data`), `-helper external` (don't spawn the helper),
`-helper-cmd "..."` (custom helper command). Run `./camweb -h` for all.

To run it as a service, see [`deploy/camweb.service`](deploy/camweb.service).

## Opening the page (HTTPS / secure context)

The browser's video decoder (WebCodecs) only works on **HTTPS or
`http://localhost`**. The app serves plain HTTP, so opening
`http://<pi-address>:8080` directly shows a warning instead of video. Pick one:

- **SSH tunnel** (works on the LAN and over the internet):

  ```bash
  ssh -L 8080:localhost:8080 pi@raspberrypi.local
  ```

  then open `http://localhost:8080`.
- **Chrome/Edge flag** for LAN testing: open
  `chrome://flags/#unsafely-treat-insecure-origin-as-secure`, add
  `http://raspberrypi.local:8080`, relaunch.
- **HTTPS tunnel/proxy** for internet access: Tailscale Serve, Cloudflare
  Tunnel, or Caddy in front of camweb. The app has no authentication, so use
  one that controls access.

## Development without a Pi

```bash
make test          # go vet, Go tests and Python helper tests
```

```bash
make run-fake      # camweb + Go fake helper replaying a test pattern
```

```bash
make run-fake-py   # camweb + the real camhelper.py on a fake Picamera2 (needs numpy, Pillow)
```

Then open `http://localhost:8080`.

## M0 hardware spike

Before relying on the numbers in the design, run the spike on the Pi (stop
camweb first; it needs the camera to itself):

```bash
make deploy PI=pi@raspberrypi.local
```

```bash
ssh pi@raspberrypi.local 'cd camweb && python3 spike/m0_spike.py all' > SPIKE.md
```

It checks one-frame-per-callback, timestamps, snapshot-while-encoding
(drops, GIL jitter), stream sizes, control latency, socket latency and
encoder limits, and prints Markdown for `docs/SPIKE.md`.

## Layout

```
cmd/camweb/        server entry point
cmd/fakecam/       fake helper (Go) + testdata/test.h264
camhelper/         camhelper.py (Picamera2 helper, embedded into camweb), tests, fake/ Picamera2
internal/proto/    IPC protocol: video framing, JSON-RPC 2.0 types
internal/helper/   sockets, session, helper process supervisor
internal/camera/   config model, libcamera control mapping, Manager
internal/stream/   hub (fan-out, drop-to-keyframe), browser wire format
internal/h264/     NAL/SPS helpers
internal/web/      HTTP server, template, static JS/CSS
spike/             M0 hardware spike script
deploy/            systemd unit
```

## Focusing the HQ lens

The **Focus** number under Exposure is a sharpness score computed by the image
processor (libcamera `FocusFoM`); the lens itself reports nothing. Turn the
focus ring until the number peaks.
- It is measured in the centre of the frame only.
- It has no absolute scale: compare values for the same scene and settings.
- Noise at high gain inflates it, so lock exposure first with *Lock current*.

The ⓘ button next to it explains the same; details and source links are in
[DESIGN.md §3.5](docs/DESIGN.md).

## Troubleshooting

- **"WebCodecs not available"**: see the secure-context section above.
- **Overlay shows `helper-restarting` / `error`**: check the log (stderr or
  `journalctl -u camweb`); lines prefixed `[helper]` come from camhelper.py.
  Make sure nothing else uses the camera (`rpicam-*`, another camweb).
- **"Failed to allocate buffers" / CMA errors**: lower *Camera buffers* in the
  UI, or raise CMA in `/boot/firmware/config.txt`
  (`dtoverlay=vc4-kms-v3d,cma-512`).
- **Video stutters while capturing**: report the M0 spike numbers for "callback
  interval during snapshots"; the fix is to move JPEG encoding to a
  subprocess (DESIGN.md §9).
