# camera_rpi

Prototype web UI and REST API for a **Raspberry Pi 4 + HQ Camera (IMX477)**:

- low-latency live H.264 stream in the browser (WebCodecs, no plugins, no JS
  libraries), with an automatic MJPEG fallback
- live camera settings: exposure, gain, white balance, image tuning, frame
  rate, stream size, bitrate
- **Capture**: a 2028×1520 JPEG taken without interrupting the video
- a [REST API](docs/REST_API.md) with everything the page uses, plus raw H.264
  and MJPEG streams and Server-Sent Events, for other clients

> **Hardware-specific.** The code is written and measured for the Pi 4 and
> the HQ camera: one sensor mode (2028×1520), 4:3 stream sizes, the Pi 4's
> hardware encoders and their quirks. The server and API are generic; the
> camera constants and explanations are not. What carries over to other Pis
> and cameras: [docs/HW_SPECIFIC.md](docs/HW_SPECIFIC.md).

```
camhelper.py (Python, Picamera2)  ──ctl.sock: JSON-RPC 2.0──▶  camweb (Go, stdlib only)  ──HTTP──▶  browser / other clients
  libcamera, HW H.264 + MJPEG     ──video.sock: framed video─▶  config, fan-out, REST API
```

## Documentation

| Document | Contents |
|---|---|
| [Architecture](docs/ARCHITECTURE.md) | components, IPC, Go server, streaming, security, latency, testing |
| [REST API](docs/REST_API.md) | HTTP endpoints, settings JSON, schema, status, events, streams, examples |
| [Helper API](docs/HELPER_API.md) | protocol between camweb and the camera helper (for writing another helper) |
| [Hardware details](docs/HARDWARE.md) | sensor mode, encoders, every setting and what libcamera does with it (focus, exposure, lux, metering, image, white balance) |
| [Hardware-specific parts](docs/HW_SPECIFIC.md) | what works on other Pis and cameras, what is Pi 4 / IMX477 only, porting checklist |
| [Spike](docs/SPIKE.md) | measurements on the real hardware and what they mean |

## Requirements

- Raspberry Pi 4, HQ Camera, Raspberry Pi OS Bookworm or newer; the camera
  shows up in `rpicam-hello --list-cameras`
- `python3-picamera2` 0.3.19 or newer (preinstalled on desktop images; on
  Lite: `sudo apt install -y python3-picamera2 --no-install-recommends`)
- To build: Go 1.22+ on any machine (the binary cross-compiles and embeds the
  Python helper)

## Quick start

Build on your computer and copy the binary to the Pi:

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

Open `http://raspberrypi.local:8080`. Over plain HTTP the page uses the MJPEG
fallback; for H.264 (lower bandwidth) set up HTTPS below.

Main flags (`./camweb -h` for all): `-hflip`, `-vflip` (both = rotate 180°),
`-data DIR` (config, default `./data`), `-captures DIR` (where a snapshot is
held until it is downloaded, then deleted; default `/dev/shm/camweb-captures`,
in RAM), `-tls-cert FILE -tls-key
FILE` (HTTPS), `-cors-origin ORIGIN` (allow a UI on another origin to use the
API), `-helper external` / `-helper-cmd "..."` (run your own helper). To run it
as a service: [deploy/camweb.service](deploy/camweb.service).

## Opening the page

The browser's H.264 decoder (WebCodecs) only works on **HTTPS or
`http://localhost`**.

**Built-in HTTPS with your own small CA** (recommended on the LAN):

1. On your computer, create a CA and a certificate for every name and IP you
   use to reach the Pi (the CA is reused on later runs, so you trust it once;
   keep `certs/ca.key` private):

   ```bash
   make cert CERT_HOSTS=raspberrypi.local,raspberrypi,192.168.1.50
   ```

2. Copy the server certificate and key to the Pi:

   ```bash
   make deploy-cert PI=pi@raspberrypi.local
   ```

3. Start camweb with TLS:

   ```bash
   ./camweb -listen :8443 -tls-cert server.crt -tls-key server.key
   ```

4. Trust `certs/ca.crt` on each device, then open
   `https://raspberrypi.local:8443`:
   - Chrome/Edge (Windows, Linux): Settings → Privacy and security → Security →
     Manage certificates → Authorities → Import
   - Firefox: Settings → Privacy & Security → Certificates → View Certificates
     → Authorities → Import (tick "trust this CA to identify websites")
   - macOS / Safari: open `ca.crt` in Keychain Access (System), set "Always
     Trust"
   - iOS: install the profile, then enable it in Settings → General → About →
     Certificate Trust Settings
   - Android: Settings → Security → Encryption & credentials → Install a
     certificate → CA certificate

A name error in the browser means the address is not in `CERT_HOSTS`: run
`make cert` again and repeat steps 2–3.

**Alternatives:** an SSH tunnel (`ssh -L 8080:localhost:8080
pi@raspberrypi.local`, then `http://localhost:8080`), or an HTTPS tunnel/proxy
for internet access (Tailscale Serve, Cloudflare Tunnel, Caddy). The app has
**no authentication**: expose it only through something that controls access.

**MJPEG fallback:** browsers that cannot decode H.264 (no WebCodecs, plain
HTTP, or Firefox for Android, whose decoder fails) get MJPEG automatically;
the note under the video says why. It uses several times the bandwidth; lower
*MJPEG fallback bitrate* or use every 2nd–3rd frame to reduce it. `?mjpeg=1`
forces it.

## Using the camera

- **Focus** (HQ lens is manual): turn the ring until the *Focus* number peaks;
  compare only for the same scene, with exposure locked.
- **Exposure:** in a dark scene auto exposure hits the frame-time and gain
  limits and EV stops working; lower the frame rate, use the *long* profile at
  ≤ 8 fps, or switch to manual.
- Every control has an ⓘ popup that explains it and names the tuning-file
  parameter behind it. Full details: [docs/HARDWARE.md](docs/HARDWARE.md).

## Other clients

```bash
curl -s http://raspberrypi.local:8080/api/status
```

```bash
mpv --profile=low-latency --untimed --demuxer-lavf-format=h264 http://raspberrypi.local:8080/stream.h264
```

```bash
ffplay -fflags nobuffer -flags low_delay -framedrop -f h264 http://raspberrypi.local:8080/stream.h264
```

```bash
curl -s -X POST http://raspberrypi.local:8080/api/capture -o snap.jpg
```

With HTTPS add `--tls-verify=no` (mpv) or `-tls_verify 0` (ffplay), or trust
your CA instead: [REST_API.md](docs/REST_API.md#streamh264).

## Using it as a webcam in video calls

camweb is not a USB webcam, but the laptop can turn `/stream.h264` into a
virtual camera that Zoom, Meet, Teams etc. list like any other. Camera
settings can still be changed on the camweb page during the call.

**Linux: v4l2loopback + ffmpeg**

1. Install the loopback module (built by DKMS, so the headers of the running
   kernel are needed) and ffmpeg:

   - Arch: `sudo pacman -S --needed v4l2loopback-dkms v4l2loopback-utils linux-headers ffmpeg`
     (`linux-lts-headers`, `linux-zen-headers`, … for other kernels)
   - Debian/Ubuntu: `sudo apt install v4l2loopback-dkms ffmpeg`

2. Create the virtual camera:

   ```bash
   sudo modprobe v4l2loopback video_nr=10 card_label="Pi HQ Camera" exclusive_caps=1
   ```

3. Feed it from the Pi and keep this running during the call:

   ```bash
   ffmpeg -fflags nobuffer -flags low_delay -f h264 -i http://raspberrypi.local:8080/stream.h264 -pix_fmt yuv420p -f v4l2 /dev/video10
   ```

   With HTTPS, put the TLS options before `-i`: `-tls_verify 1 -ca_file
   certs/ca.crt` (the address must be one of the certificate's names,
   `CERT_HOSTS` in `make cert`) or `-tls_verify 0` to skip the check:

   ```bash
   ffmpeg -tls_verify 1 -ca_file certs/ca.crt -fflags nobuffer -flags low_delay -f h264 -i https://raspberrypi.local:8443/stream.h264 -pix_fmt yuv420p -f v4l2 /dev/video10
   ```

4. Choose **Pi HQ Camera** in the meeting app.

To create the camera at every boot:

```bash
echo v4l2loopback | sudo tee /etc/modules-load.d/v4l2loopback.conf
```

```bash
echo 'options v4l2loopback video_nr=10 card_label="Pi HQ Camera" exclusive_caps=1' | sudo tee /etc/modprobe.d/v4l2loopback.conf
```

**Windows, macOS: OBS Studio.** Add a *Media Source*, untick *Local file*,
enter the `/stream.h264` address with input format `h264` (or use
`/stream.mjpeg`), turn off buffering, then click *Start Virtual Camera* and
choose *OBS Virtual Camera* in the meeting app.

Notes:

- **No audio:** use the laptop's microphone.
- **4:3 picture:** all stream sizes are 4:3; meeting apps crop or pillarbox
  it to 16:9.
- `modprobe: FATAL: Module v4l2loopback not found`: the module was not built
  for the running kernel. Check `dkms status`; after a kernel update without
  a reboot, reboot first.
- `Peer certificate failed verification`: ffmpeg does not trust your CA or
  the address is not in the certificate; use `-ca_file certs/ca.crt` with a
  name from `CERT_HOSTS`, or `-tls_verify 0`.

## Development without a Pi

```bash
make test          # go vet, Go tests, Python helper tests
```

```bash
make run-fake      # camweb + Go fake helper replaying a test stream
```

```bash
make run-fake-py   # camweb + the real camhelper.py on a fake Picamera2 (needs numpy, Pillow)
```

Then open `http://localhost:8080`. Hardware measurements: run
`spike/m0_spike.py` on the Pi ([docs/SPIKE.md](docs/SPIKE.md)).

## Troubleshooting

- **Overlay shows `helper-restarting` or `error`:** check the log (stderr or
  `journalctl -u camweb`; `[helper]` lines come from camhelper.py) and that
  nothing else uses the camera (`rpicam-*`, another camweb).
- **"Failed to allocate buffers" / CMA errors:** lower *Camera buffers*, or
  raise CMA in `/boot/firmware/config.txt` (`dtoverlay=vc4-kms-v3d,cma-512`).
- **Certificate error:** `ca.crt` not trusted on that device, or the address
  is missing from `CERT_HOSTS`.
- **Page shows MJPEG instead of H.264:** the note under the video gives the
  reason (usually plain HTTP).
