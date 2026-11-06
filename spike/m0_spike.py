#!/usr/bin/env python3
"""M0 hardware spike for camera_rpi (docs/PLAN.md, M0).

Run on the Raspberry Pi 4 with the HQ camera, with nothing else using the
camera (stop camweb first):

    python3 spike/m0_spike.py all            # everything, ~2 minutes
    python3 spike/m0_spike.py info encode    # selected experiments

Prints Markdown; paste the output into docs/SPIKE.md. Images from the "sizes"
experiment are written to ./spike_out/.

Experiments (PLAN.md M0 numbering):
  info     0.1  sensor modes, controls, properties, enum names, colour space
  encode   0.2-0.5  one AU per callback? keyframes/SPS, timestamp meaning,
           capture-to-callback latency, snapshots while encoding (drops,
           copy/encode time, callback jitter = GIL impact)
  sizes    0.6  lores sizes in the 2028x1520 mode: applied sizes/strides,
           saved images for an aspect check
  controls 0.7  frames until manual exposure applies; return to auto
  socket   0.8  Unix socket hand-off latency for 200 KB frames
  limits   0.9  1440x1080@40 at several bitrates: delivered fps, CPU
"""
import os
import socket
import statistics
import sys
import threading
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "camhelper"))

SENSOR = {"output_size": (2028, 1520), "bit_depth": 12}
MAIN = (2028, 1520)
LORES = (1440, 1080)
OUT = "spike_out"


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(len(xs) * p / 100))] if xs else float("nan")


def summary(xs, unit=""):
    if not xs:
        return "n/a"
    return "mean %.2f%s, p50 %.2f, p95 %.2f, max %.2f (n=%d)" % (
        statistics.mean(xs), unit, pct(xs, 50), pct(xs, 95), max(xs), len(xs))


def nal_types(data):
    out = []
    i = data.find(b"\x00\x00\x01")
    while i >= 0 and i + 3 < len(data):
        t = data[i + 3] & 0x1F
        first_mb_zero = bool(i + 4 < len(data) and data[i + 4] & 0x80)
        out.append((t, first_mb_zero))
        i = data.find(b"\x00\x00\x01", i + 3)
    return out


def cpu_seconds():
    with open("/proc/self/stat") as f:
        parts = f.read().rsplit(")", 1)[1].split()
    return (int(parts[11]) + int(parts[12])) / os.sysconf("SC_CLK_TCK")


def video_config(picam2, lores=LORES, fps=30, buffer_count=6):
    d = int(1e6 / fps)
    return picam2.create_video_configuration(
        main={"size": MAIN, "format": "YUV420"},
        lores={"size": lores, "format": "YUV420"},
        sensor=SENSOR, buffer_count=buffer_count, display=None, encode="lores",
        controls={"FrameDurationLimits": (d, d)})


class Recorder:
    """Output that records every encoder callback."""

    def __init__(self):
        from picamera2.outputs import Output
        rec = self

        class Out(Output):
            def outputframe(self, frame, keyframe=True, timestamp=None, *args, **kwargs):
                rec.on(frame, keyframe, timestamp, args, kwargs)

        self.output = Out()
        self.frames = []  # (mono_ns, len, keyframe, timestamp, vcl_pictures, has_sps)
        self.extra = None

    def on(self, frame, keyframe, timestamp, args, kwargs):
        now = time.monotonic_ns()
        data = bytes(frame)
        types = nal_types(data)
        pictures = sum(1 for t, first in types if 1 <= t <= 5 and first)
        self.frames.append((now, len(data), bool(keyframe), timestamp, pictures, any(t == 7 for t, _ in types)))
        if self.extra is None:
            self.extra = (len(args), sorted(kwargs))


# ------------------------------------------------------------------ 0.1

def exp_info(picam2):
    import picamera2
    print("## 0.1 Camera info\n")
    print("- picamera2 version: `%s`" % getattr(picamera2, "__version__", "?"))
    try:
        import libcamera
        print("- libcamera version: `%s`" % getattr(libcamera, "__version__", "?"))
    except Exception as e:  # noqa: BLE001
        print("- libcamera import failed: %s" % e)
    print("- properties: `%s`\n" % {k: v for k, v in picam2.camera_properties.items() if k in ("Model", "PixelArraySize", "ScalerCropMaximum", "UnitCellSize")})
    print("| size | bit depth | fps | crop limits | exposure limits |\n|---|---|---|---|---|")
    for m in picam2.sensor_modes:
        print("| %s | %s | %s | %s | %s |" % (m.get("size"), m.get("bit_depth"), m.get("fps"), m.get("crop_limits"), m.get("exposure_limits")))
    print("\nControls (min, max, default):\n")
    for k, v in sorted(picam2.camera_controls.items()):
        print("- `%s`: %s" % (k, v))
    import camhelper
    print("\nEnum names:\n")
    for name in ("AeMeteringMode", "AeExposureMode", "AwbMode", "NoiseReductionMode", "ExposureTimeMode", "AnalogueGainMode"):
        print("- `%s`: %s" % (name, camhelper._enum_values(name)))
    cfg = video_config(picam2)
    picam2.configure(cfg)
    applied = picam2.camera_configuration()
    print("\n- video config colour space: `%s`" % applied.get("colour_space"))
    print("- applied main: `%s`, lores: `%s`, sensor: `%s`\n" % (applied["main"], applied["lores"], applied.get("sensor")))


# ------------------------------------------------------------------ 0.2-0.5

def exp_encode(picam2, seconds=8, snapshots=10):
    from picamera2.encoders import H264Encoder
    import camhelper
    print("## 0.2-0.5 Encoding, latency, snapshots while encoding\n")
    picam2.configure(video_config(picam2))
    sensor_ts = {}

    def post(req):
        md = req.get_metadata()
        sensor_ts[md.get("SensorTimestamp")] = time.monotonic_ns()
    picam2.post_callback = post
    rec = Recorder()
    try:
        enc = H264Encoder(bitrate=6_000_000, repeat=True, iperiod=30, profile="high")
    except TypeError:
        enc = H264Encoder(bitrate=6_000_000, repeat=True, iperiod=30)
        print("- NOTE: H264Encoder has no `profile` argument in this version")
    cpu0 = cpu_seconds()
    picam2.start_encoder(enc, rec.output, name="lores")
    picam2.start()
    time.sleep(seconds)
    baseline_n = len(rec.frames)
    cpu_base = (cpu_seconds() - cpu0) / seconds

    # snapshots while encoding
    cfg = picam2.camera_configuration()
    full_range = "sycc" in str(cfg.get("colour_space", "")).lower()
    copy_ms, enc_ms = [], []
    t_snap0 = time.monotonic_ns()
    for i in range(snapshots):
        t0 = time.monotonic()
        req = picam2.capture_request()
        try:
            arr = req.make_array("main").copy()
        finally:
            req.release()
        t1 = time.monotonic()
        th = threading.Thread(target=lambda: camhelper.encode_yuv420_jpeg(arr, MAIN[0], MAIN[1], 95, full_range))
        th.start()
        th.join()
        t2 = time.monotonic()
        copy_ms.append((t1 - t0) * 1000)
        enc_ms.append((t2 - t1) * 1000)
        time.sleep(0.2)
    t_snap1 = time.monotonic_ns()
    time.sleep(1)
    picam2.stop_encoder(enc)
    picam2.stop()
    picam2.post_callback = None

    fr = rec.frames
    print("- outputframe extra args: %s positional, kwargs %s" % rec.extra if rec.extra else "- no frames!")
    multi = sum(1 for f in fr if f[4] != 1)
    print("- callbacks: %d; callbacks with != 1 picture (should be 0): **%d**" % (len(fr), multi))
    keys = [f for f in fr if f[2]]
    print("- keyframes: %d; keyframes with SPS: %d; non-key with SPS: %d" % (len(keys), sum(f[5] for f in keys), sum(f[5] for f in fr if not f[2])))
    print("- first callback keyframe: %s" % (fr[0][2] if fr else None))
    ts = [f[3] for f in fr if f[3] is not None]
    if ts:
        print("- `timestamp` argument sample: %s (type %s)" % (ts[:3], type(ts[0]).__name__))
        st = sorted(k for k in sensor_ts if k)
        if st:
            print("- SensorTimestamp sample: %s" % st[:3])
            print("- timestamp*1000 == SensorTimestamp for %d of %d frames" % (sum(1 for t in ts if t * 1000 in sensor_ts), len(ts)))
    # capture -> callback latency (only if same clock domain)
    lat = []
    for f in fr:
        if f[3] is not None:
            st = f[3] * 1000
            if st in sensor_ts and abs(f[0] - st) < 5e9:
                lat.append((f[0] - st) / 1e6)
    print("- sensor timestamp -> outputframe (if monotonic clock): %s" % summary(lat, " ms"))
    post_lat = [(v - k) / 1e6 for k, v in sensor_ts.items() if k and abs(v - k) < 5e9]
    print("- sensor timestamp -> post_callback: %s" % summary(post_lat, " ms"))
    iv = [(fr[i][0] - fr[i - 1][0]) / 1e6 for i in range(1, baseline_n)]
    iv_snap = [(fr[i][0] - fr[i - 1][0]) / 1e6 for i in range(1, len(fr)) if t_snap0 <= fr[i][0] <= t_snap1]
    print("- callback interval, baseline: %s" % summary(iv, " ms"))
    print("- callback interval during snapshots (GIL impact): %s" % summary(iv_snap, " ms"))
    gaps = [t for t in (
        (ts[i] - ts[i - 1]) for i in range(1, len(ts))) if t > 1.5 * 1e6 / 30]
    print("- timestamp gaps > 1.5 frame (dropped frames): **%d**" % len(gaps))
    print("- snapshot copy (capture_request+copy): %s" % summary(copy_ms, " ms"))
    print("- snapshot JPEG encode 2028x1520: %s" % summary(enc_ms, " ms"))
    print("- process CPU while encoding: %.0f%% of one core\n" % (cpu_base * 100))


# ------------------------------------------------------------------ 0.6

def exp_sizes(picam2):
    import camhelper
    print("## 0.6 Stream sizes in the 2028x1520 mode\n")
    os.makedirs(OUT, exist_ok=True)
    print("| requested lores | applied lores | stride | main | ScalerCrop | files |\n|---|---|---|---|---|---|")
    for lores in ((1440, 1080), (1024, 768), (640, 480)):
        picam2.configure(video_config(picam2, lores=lores))
        picam2.start()
        time.sleep(1.5)
        req = picam2.capture_request()
        try:
            lo = req.make_array("lores").copy()
            mn = req.make_array("main").copy()
            md = req.get_metadata()
        finally:
            req.release()
        picam2.stop()
        cfg = picam2.camera_configuration()
        fr = "sycc" in str(cfg.get("colour_space", "")).lower()
        lw, lh = cfg["lores"]["size"]
        names = []
        for name, arr, (w, h) in (("lores", lo, (lw, lh)), ("main", mn, MAIN)):
            fn = os.path.join(OUT, "%s_%dx%d.jpg" % (name, lores[0], lores[1]))
            with open(fn, "wb") as f:
                f.write(camhelper.encode_yuv420_jpeg(arr, w, h, 90, fr))
            names.append(fn)
        print("| %s | %s | %s | %s | %s | %s |" % (lores, cfg["lores"]["size"], cfg["lores"].get("stride"), cfg["main"]["size"], md.get("ScalerCrop"), ", ".join(names)))
    print("\nCheck the images: a circle must stay round in both lores and main.\n")


# ------------------------------------------------------------------ 0.7

def exp_controls(picam2):
    print("## 0.7 Controls\n")
    picam2.configure(video_config(picam2))
    mds = []
    picam2.post_callback = lambda r: mds.append(r.get_metadata())
    picam2.start()
    time.sleep(2)
    auto_exp = mds[-1].get("ExposureTime")
    print("- auto exposure: ExposureTime=%s AnalogueGain=%s" % (auto_exp, mds[-1].get("AnalogueGain")))
    target = 5000 if (auto_exp or 0) > 7000 else 12000
    n0 = len(mds)
    picam2.set_controls({"AeEnable": False, "ExposureTime": target, "AnalogueGain": 2.0})
    t0 = time.monotonic()
    while time.monotonic() - t0 < 3:
        if any(abs((m.get("ExposureTime") or 0) - target) < target * 0.05 for m in mds[n0:]):
            break
        time.sleep(0.01)
    hit = next((i for i, m in enumerate(mds[n0:]) if abs((m.get("ExposureTime") or 0) - target) < target * 0.05), None)
    print("- manual ExposureTime=%d applied after **%s** frames" % (target, hit))
    time.sleep(1)
    # back to auto, variant A (older RPi semantics)
    known = picam2.camera_controls
    if "ExposureTimeMode" in known:
        picam2.set_controls({"AeEnable": True, "ExposureTimeMode": 0, "AnalogueGainMode": 0})
        variant = "ExposureTimeMode/AnalogueGainMode = 0"
    else:
        picam2.set_controls({"AeEnable": True, "ExposureTime": 0, "AnalogueGain": 0.0})
        variant = "AeEnable + ExposureTime=0 + AnalogueGain=0"
    time.sleep(2)
    print("- back to auto via %s: ExposureTime=%s AnalogueGain=%s (should differ from %d/2.0 unless the scene matches)" % (
        variant, mds[-1].get("ExposureTime"), mds[-1].get("AnalogueGain"), target))
    n0 = len(mds)
    picam2.set_controls({"FrameDurationLimits": (50000, 50000)})
    time.sleep(1.5)
    fd = [m.get("FrameDuration") for m in mds[n0:]][-5:]
    print("- FrameDurationLimits -> 50000 at runtime: last FrameDuration values %s" % fd)
    picam2.stop()
    picam2.post_callback = None
    print()


# ------------------------------------------------------------------ 0.8

def exp_socket(n=300, size=200_000):
    print("## 0.8 Unix socket hand-off\n")
    a, b = socket.socketpair(socket.AF_UNIX, socket.SOCK_STREAM)
    a.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 1 << 20)
    lat = []

    def reader():
        for _ in range(n):
            hdr = b.recv(8, socket.MSG_WAITALL)
            sent = int.from_bytes(hdr, "big")
            remaining = size
            while remaining:
                remaining -= len(b.recv(min(remaining, 1 << 20)))
            lat.append((time.monotonic_ns() - sent) / 1e6)
    th = threading.Thread(target=reader)
    th.start()
    payload = b"\x00" * size
    for _ in range(n):
        a.sendall(time.monotonic_ns().to_bytes(8, "big") + payload)
        time.sleep(1 / 60)
    th.join()
    print("- %d KB messages, sender -> reader complete: %s\n" % (size // 1000, summary(lat, " ms")))


# ------------------------------------------------------------------ 0.9

def exp_limits(picam2, fps=40, seconds=6):
    from picamera2.encoders import H264Encoder
    print("## 0.9 Encoder limits (1440x1080@%d)\n" % fps)
    print("| bitrate | delivered fps | Mbit/s | CPU % |\n|---|---|---|---|")
    for br in (2_000_000, 6_000_000, 12_000_000):
        picam2.configure(video_config(picam2, fps=fps))
        rec = Recorder()
        enc = H264Encoder(bitrate=br, repeat=True, iperiod=fps)
        c0 = cpu_seconds()
        picam2.start_encoder(enc, rec.output, name="lores")
        picam2.start()
        time.sleep(1)
        n0, b0, t0 = len(rec.frames), sum(f[1] for f in rec.frames), time.monotonic()
        time.sleep(seconds)
        n1, b1, t1 = len(rec.frames), sum(f[1] for f in rec.frames), time.monotonic()
        cpu = (cpu_seconds() - c0) / (seconds + 1)
        picam2.stop_encoder(enc)
        picam2.stop()
        print("| %d M | %.1f | %.2f | %.0f |" % (br // 1_000_000, (n1 - n0) / (t1 - t0), (b1 - b0) * 8 / (t1 - t0) / 1e6, cpu * 100))
    print()


EXPERIMENTS = ["info", "encode", "sizes", "controls", "socket", "limits"]


def main():
    want = sys.argv[1:] or ["all"]
    if "all" in want:
        want = EXPERIMENTS
    bad = [w for w in want if w not in EXPERIMENTS]
    if bad:
        sys.exit("unknown experiment(s) %s; choose from %s or all" % (bad, EXPERIMENTS))
    print("# M0 spike results (%s)\n" % time.strftime("%Y-%m-%d %H:%M"))
    if want == ["socket"]:
        exp_socket()
        return
    from picamera2 import Picamera2
    picam2 = Picamera2()
    try:
        for w in want:
            if w == "socket":
                exp_socket()
            else:
                globals()["exp_" + w](picam2)
            sys.stdout.flush()
    finally:
        picam2.close()


if __name__ == "__main__":
    main()
