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
captures, default `./data`), `-tls-cert FILE -tls-key FILE` (serve HTTPS, see
below), `-helper external` (don't spawn the helper), `-helper-cmd "..."`
(custom helper command). Run `./camweb -h` for all.

To run it as a service, see [`deploy/camweb.service`](deploy/camweb.service).

## Opening the page (HTTPS / secure context)

The browser's video decoder (WebCodecs) only works on **HTTPS or
`http://localhost`**. Over plain HTTP, opening `http://<pi-address>:8080`
shows a warning instead of video.

### Built-in HTTPS with your own small CA (recommended on the LAN)

1. On your computer, create a private CA and a certificate for the names and
   IPs you use to reach the Pi:

   ```bash
   make cert CERT_HOSTS=raspberrypi.local,raspberrypi,192.168.1.50
   ```

   This writes `certs/ca.crt`, `certs/ca.key`, `certs/server.crt` and
   `certs/server.key`. The CA is created once and reused when you run
   `make cert` again (for example after the Pi gets a new IP), so you install
   it only once. Keep `ca.key` private: anyone with it can make certificates
   your browser will trust.
2. Copy the server certificate and key to the Pi:

   ```bash
   make deploy-cert PI=pi@raspberrypi.local
   ```

3. Start camweb with TLS:

   ```bash
   ./camweb -listen :8443 -tls-cert server.crt -tls-key server.key
   ```

4. Trust `certs/ca.crt` once on each device that opens the page, then open
   `https://raspberrypi.local:8443`:
   - **Chrome/Edge (Windows, Linux):** Settings → Privacy and security →
     Security → Manage certificates → Authorities → Import.
   - **Firefox:** Settings → Privacy & Security → Certificates → View
     Certificates → Authorities → Import (tick "trust this CA to identify
     websites").
   - **macOS / Safari:** open `ca.crt` in Keychain Access (System keychain),
     then set it to "Always Trust".
   - **iOS:** install the profile, then enable it under Settings → General →
     About → Certificate Trust Settings.
   - **Android:** Settings → Security → Encryption & credentials → Install a
     certificate → CA certificate.

The certificate only works for the names and IPs listed in `CERT_HOSTS`. If
the browser shows a name error, run `make cert` again with the address you use
and repeat steps 2–3.

### Other options

- **SSH tunnel** (works on the LAN and over the internet, no certificates):

  ```bash
  ssh -L 8080:localhost:8080 pi@raspberrypi.local
  ```

  then open `http://localhost:8080`.
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
cmd/certgen/       private CA + server certificate for HTTPS (make cert)
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

## Exposure: EV, frame rate, shutter, gain

- **Auto mode:** brightness target × 2^EV, then shutter (up to the frame time
  and the profile's maximum), then gain (up to the profile's maximum, 16 for
  the HQ camera).
- In a dark scene auto exposure hits both limits, and EV then has no effect.
  The UI warns about this under *Actual values*. Lower the frame rate, use the
  "long" profile at ≤ 8 fps, or switch to manual.
- **Manual mode** uses shutter and gain as set. EV, metering and profile are
  disabled.
- **Metering** chooses which of the 15 Pi 4 zones count when auto exposure
  judges brightness: centre-weighted, spot or matrix. The ⓘ popup shows the
  zone layout.

Each control has an ⓘ popup. Popups explain the behaviour in general and name
the parameter in the camera's libcamera tuning file that sets the actual
values, for example `rpi.agc → exposure_modes`. libcamera logs which file it
uses ("Using tuning file …", in the camweb log); for the HQ camera on a Pi 4 it
is usually `/usr/share/libcamera/ipa/rpi/vc4/imx477.json`. Details with source
links are in [DESIGN.md §3.6](docs/DESIGN.md).

**Image** settings (brightness, contrast, saturation, sharpness, denoise) only
change how the picture is rendered, not the exposure. Denoise "auto" is
Picamera2's video default, *fast*: spatial plus fast colour denoise. See
DESIGN.md §3.9.

**White balance:** auto picks a colour temperature within the selected mode's
range (`rpi.awb → modes`), guided by the scene and the lux level. Manual uses
red/blue gains; *Lock current* copies the auto gains (DESIGN.md §3.10).

## Troubleshooting

- **"WebCodecs not available"**: see the secure-context section above.
- **Certificate error in the browser**: either `ca.crt` is not trusted on that
  device, or the address is not in `CERT_HOSTS` (run `make cert` again).
- **Overlay shows `helper-restarting` / `error`**: check the log (stderr or
  `journalctl -u camweb`); lines prefixed `[helper]` come from camhelper.py.
  Make sure nothing else uses the camera (`rpicam-*`, another camweb).
- **"Failed to allocate buffers" / CMA errors**: lower *Camera buffers* in the
  UI, or raise CMA in `/boot/firmware/config.txt`
  (`dtoverlay=vc4-kms-v3d,cma-512`).
- **Video stutters while capturing**: report the M0 spike numbers for "callback
  interval during snapshots"; the fix is to move JPEG encoding to a
  subprocess (DESIGN.md §9).
