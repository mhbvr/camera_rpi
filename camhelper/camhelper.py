#!/usr/bin/env python3
"""camhelper: Picamera2 camera helper for camweb.

Implements docs/PROTOCOL.md v1:
  * ctl.sock   - JSON-RPC 2.0, one message per line (server -> requests,
                 helper -> responses and notifications)
  * video.sock - 32-byte big-endian header + one H.264 access unit (Annex-B)

The helper is deliberately thin: it owns the camera and does only what needs
libcamera. All policy (config model, validation, control mapping) lives in the
Go server. Logs go to stderr; stdout is unused.

Dependencies: Raspberry Pi OS python3-picamera2 (with numpy, simplejpeg).
"""

import argparse
import json
import logging
import os
import signal
import socket
import struct
import sys
import threading
import time
import traceback

PROTO_VERSION = 1

# video.sock framing
HEADER = struct.Struct(">IBBHQQQ")  # len, type, flags, reserved, seq, pts_us, wall_us
assert HEADER.size == 32
T_AU, T_STREAM_START, T_STREAM_END = 1, 2, 3
F_KEY, F_CONFIG = 1, 2

# JSON-RPC error codes (PROTOCOL.md §3.1)
E_PARSE, E_INVALID_REQUEST, E_METHOD_NOT_FOUND, E_INVALID_PARAMS, E_INTERNAL = (
    -32700, -32600, -32601, -32602, -32603)
E_BUSY, E_NOT_CONFIGURED, E_CAMERA, E_IO = -32001, -32002, -32003, -32004

MAX_LINE = 1 << 20
METADATA_KEYS = ("ExposureTime", "AnalogueGain", "DigitalGain", "ColourGains",
                 "ColourTemperature", "Lux", "FrameDuration", "FocusFoM",
                 "SensorTimestamp")

log = logging.getLogger("camhelper")


class RpcError(Exception):
    def __init__(self, code, message, data=None):
        super().__init__(message)
        self.code, self.message, self.data = code, message, data


def _json_default(o):
    """Make libcamera/numpy values JSON-serialisable."""
    for attr in ("tolist", "item"):
        if hasattr(o, attr):
            return getattr(o, attr)()
    if isinstance(o, (set, frozenset)):
        return list(o)
    return str(o)


# ---------------------------------------------------------------- transport

class Ctl:
    """JSON-RPC 2.0 over a stream socket, newline-delimited."""

    def __init__(self, sock):
        self.sock = sock
        self.rfile = sock.makefile("rb")
        self.lock = threading.Lock()

    def _send(self, obj):
        data = json.dumps(obj, separators=(",", ":"), default=_json_default).encode() + b"\n"
        with self.lock:
            self.sock.sendall(data)

    def notify(self, method, params=None):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        try:
            self._send(msg)
        except OSError as e:
            log.warning("notify %s failed: %s", method, e)

    def reply(self, id_, result):
        self._send({"jsonrpc": "2.0", "id": id_, "result": {} if result is None else result})

    def reply_error(self, id_, code, message, data=None):
        err = {"code": code, "message": message}
        if data is not None:
            err["data"] = data
        self._send({"jsonrpc": "2.0", "id": id_, "error": err})

    def lines(self):
        while True:
            line = self.rfile.readline(MAX_LINE + 1)
            if not line:
                return
            if len(line) > MAX_LINE:
                raise RpcError(E_INVALID_REQUEST, "line too long")
            yield line


class Video:
    """Writes framed messages to video.sock. No queue: writes are direct."""

    def __init__(self, sock):
        self.sock = sock
        self.lock = threading.Lock()

    def send(self, type_, flags, seq, pts_us, payload):
        hdr = HEADER.pack(len(payload), type_, flags, 0, seq, pts_us, time.time_ns() // 1000)
        with self.lock:
            self.sock.sendall(hdr + payload)

    def send_json(self, type_, obj):
        self.send(type_, 0, 0, 0, json.dumps(obj, separators=(",", ":")).encode())


def has_sps(data):
    """True if an Annex-B buffer contains an SPS NAL unit."""
    i = data.find(b"\x00\x00\x01")
    while i >= 0 and i + 3 < len(data):
        if data[i + 3] & 0x1F == 7:
            return True
        i = data.find(b"\x00\x00\x01", i + 3)
    return False


def connect(path, timeout=10.0):
    deadline = time.monotonic() + timeout
    while True:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            s.connect(path)
            return s
        except OSError:
            s.close()
            if time.monotonic() > deadline:
                raise
            time.sleep(0.1)


# ---------------------------------------------------------------- JPEG

def encode_yuv420_jpeg(arr, width, height, quality, full_range):
    """Encode a Picamera2 YUV420 array (shape (h*3/2, stride), with padding)
    as JPEG. Limited-range video YUV is expanded to full range first, since
    JPEG uses full-range YCbCr."""
    import numpy as np
    stride = arr.shape[1]
    flat = arr.reshape(-1)
    y = arr[:height, :width]
    cstride = stride // 2
    csize = (height // 2) * cstride
    base = height * stride
    u = flat[base:base + csize].reshape(height // 2, cstride)[:, :width // 2]
    v = flat[base + csize:base + 2 * csize].reshape(height // 2, cstride)[:, :width // 2]
    if not full_range:
        y = np.clip((y.astype(np.int16) - 16) * 255 // 219, 0, 255).astype(np.uint8)
        u = np.clip((u.astype(np.int16) - 128) * 255 // 224 + 128, 0, 255).astype(np.uint8)
        v = np.clip((v.astype(np.int16) - 128) * 255 // 224 + 128, 0, 255).astype(np.uint8)
    y, u, v = (np.ascontiguousarray(p) for p in (y, u, v))
    try:
        import simplejpeg
        return simplejpeg.encode_jpeg_yuv_planes(y, u, v, quality=quality)
    except (ImportError, AttributeError):
        pass
    # Fallback: YCbCr -> RGB in numpy, JPEG with Pillow.
    from PIL import Image
    import io
    uu = u.repeat(2, axis=0).repeat(2, axis=1)[:height, :width].astype(np.float32) - 128
    vv = v.repeat(2, axis=0).repeat(2, axis=1)[:height, :width].astype(np.float32) - 128
    yy = y.astype(np.float32)
    rgb = np.stack([yy + 1.402 * vv, yy - 0.344136 * uu - 0.714136 * vv, yy + 1.772 * uu], axis=-1)
    buf = io.BytesIO()
    Image.fromarray(np.clip(rgb, 0, 255).astype(np.uint8)).save(buf, "JPEG", quality=quality)
    return buf.getvalue()


# ---------------------------------------------------------------- camera

def _lc_value(v):
    """JSON lists -> tuples (libcamera array controls want sequences)."""
    if isinstance(v, list):
        return tuple(_lc_value(x) for x in v)
    return v


class Camera:
    def __init__(self, ctl, video):
        from picamera2 import Picamera2
        self.ctl, self.video = ctl, video
        self.picam2 = Picamera2()
        self.lock = threading.Lock()  # one camera operation at a time
        self.streaming = False
        self.encoder = None
        self.output = None
        self.enc_params = {}
        self.lores_size = (0, 0)
        self.fps = 0.0
        self.session = 0
        self.seq = 0
        self.frames = 0
        self.metadata_hz = 2.0
        self._last_md = 0.0
        self.video_failed = False
        self.picam2.post_callback = self._post_callback

    # -- info

    def hello(self):
        import picamera2
        modes = []
        try:
            for m in self.picam2.sensor_modes:
                modes.append({
                    "size": list(m.get("size", (0, 0))),
                    "bit_depth": m.get("bit_depth"),
                    "fps": m.get("fps"),
                    "crop_limits": list(m.get("crop_limits", (0, 0, 0, 0))),
                    "exposure_limits": list(m.get("exposure_limits", ()) or ()),
                })
        except Exception as e:  # noqa: BLE001 - informational only
            log.warning("sensor_modes failed: %s", e)
        controls = {}
        for name, rng in self.picam2.camera_controls.items():
            mn, mx, df = (list(rng) + [None, None, None])[:3]
            info = {"min": mn, "max": mx, "default": df}
            vals = _enum_values(name)
            if vals:
                info["values"] = vals
            controls[name] = info
        props = self.picam2.camera_properties
        try:
            lc_version = __import__("libcamera").__version__
        except Exception:  # noqa: BLE001
            lc_version = ""
        return {
            "proto": PROTO_VERSION,
            "impl": "picamera2",
            "impl_version": getattr(picamera2, "__version__", ""),
            "libcamera": lc_version,
            "camera": {
                "model": props.get("Model", ""),
                "pixel_array": list(props.get("PixelArraySize", (0, 0))),
                "scaler_crop_max": list(props.get("ScalerCropMaximum", (0, 0, 0, 0))),
                "sensor_modes": modes,
                "controls": controls,
            },
            "features": ["snapshot", "metadata", "set_encoder"],
        }

    def state(self, state, detail=None):
        p = {"state": state}
        if detail:
            p["detail"] = detail
        self.ctl.notify("state", p)

    # -- operations (called with self.lock held unless noted)

    def configure(self, p):
        from libcamera import Transform
        try:
            sensor, main, lores = p["sensor"], p["main"], p["lores"]
            enc = p["encoder"]
            controls = {k: _lc_value(v) for k, v in (p.get("controls") or {}).items()}
            transform = p.get("transform") or {}
            buffer_count = int(p.get("buffer_count", 6))
        except (KeyError, TypeError) as e:
            raise RpcError(E_INVALID_PARAMS, "bad configure params: %s" % e)
        self._check_controls(controls)
        if self.streaming:
            self.state("reconfiguring")
            self._stop_stream("reconfigure")
        else:
            self.state("starting")
        cfg = self.picam2.create_video_configuration(
            main={"size": tuple(main["size"]), "format": main.get("format", "YUV420")},
            lores={"size": tuple(lores["size"]), "format": lores.get("format", "YUV420")},
            sensor={"output_size": tuple(sensor["size"]), "bit_depth": int(sensor["bit_depth"])},
            transform=Transform(hflip=int(bool(transform.get("hflip"))), vflip=int(bool(transform.get("vflip")))),
            buffer_count=buffer_count,
            controls=controls,
            display=None,
            encode="lores",
        )
        self.picam2.configure(cfg)
        self.metadata_hz = float(p.get("metadata_hz", 2))
        self.enc_params = {"bitrate": int(enc["bitrate"]), "iperiod": int(enc["iperiod"])}
        self.lores_size = tuple(lores["size"])
        fdl = controls.get("FrameDurationLimits")
        self.fps = round(1e6 / fdl[0], 3) if fdl else 0.0
        self._start_encoder()
        self.picam2.start()
        self.streaming = True
        self.state("streaming")
        applied = self.picam2.camera_configuration()
        return {"applied": {k: _stream_info(applied.get(k)) for k in ("main", "lores", "sensor")},
                "session": self.session}

    def set_encoder(self, p):
        if not self.streaming:
            raise RpcError(E_NOT_CONFIGURED, "not streaming")
        try:
            self.enc_params = {"bitrate": int(p["bitrate"]), "iperiod": int(p["iperiod"])}
        except (KeyError, TypeError, ValueError) as e:
            raise RpcError(E_INVALID_PARAMS, "bad set_encoder params: %s" % e)
        self._stop_encoder("encoder")
        self._start_encoder()
        return {"session": self.session}

    def set_controls(self, p):
        """Live controls; does not take self.lock (handled inline)."""
        try:
            controls = {k: _lc_value(v) for k, v in p["controls"].items()}
        except (KeyError, TypeError, AttributeError) as e:
            raise RpcError(E_INVALID_PARAMS, "bad set_controls params: %s" % e)
        self._check_controls(controls)
        if "FrameDurationLimits" in controls:
            self.fps = round(1e6 / controls["FrameDurationLimits"][0], 3)
        self.picam2.set_controls(controls)
        return {}

    def snapshot(self, p):
        """Capture from the running main stream. The lock is held only while
        a request is captured and copied; encoding happens after."""
        try:
            path, quality = str(p["path"]), int(p.get("quality", 95))
        except (KeyError, TypeError, ValueError) as e:
            raise RpcError(E_INVALID_PARAMS, "bad snapshot params: %s" % e)
        if not path or not 1 <= quality <= 100:
            raise RpcError(E_INVALID_PARAMS, "path required, quality 1..100", {"field": "quality"})
        t0 = time.monotonic()
        with self._camera_op():
            if not self.streaming:
                raise RpcError(E_NOT_CONFIGURED, "not streaming")
            cfg = self.picam2.camera_configuration()
            main = cfg["main"]
            full_range = "sycc" in str(cfg.get("colour_space", "")).lower()
            req = self.picam2.capture_request()
            try:
                arr = req.make_array("main").copy()
                md = req.get_metadata()
            finally:
                req.release()
        t1 = time.monotonic()
        w, h = main["size"]
        jpeg = encode_yuv420_jpeg(arr, w, h, quality, full_range)
        t2 = time.monotonic()
        final = path + ".jpg"
        try:
            with open(final + ".tmp", "wb") as f:
                f.write(jpeg)
            os.replace(final + ".tmp", final)
        except OSError as e:
            raise RpcError(E_IO, "writing %s: %s" % (final, e))
        return {
            "file": {"path": final, "size": len(jpeg), "width": w, "height": h},
            "metadata": {k: md[k] for k in METADATA_KEYS if k in md},
            "timing_ms": {"capture": round((t1 - t0) * 1000, 1), "encode": round((t2 - t1) * 1000, 1)},
        }

    def stop(self, _p=None):
        if self.streaming:
            self._stop_stream("stop")
        self.state("idle")
        return {}

    def close(self):
        try:
            if self.streaming:
                self._stop_stream("stop")
        except Exception as e:  # noqa: BLE001
            log.warning("stop on close: %s", e)
        try:
            self.picam2.close()
        except Exception as e:  # noqa: BLE001
            log.warning("close: %s", e)

    # -- internals

    def _camera_op(self):
        if not self.lock.acquire(blocking=False):
            raise RpcError(E_BUSY, "another camera operation is running")
        return _Release(self.lock)

    def _check_controls(self, controls):
        known = self.picam2.camera_controls
        unknown = [k for k in controls if k not in known]
        if unknown:
            raise RpcError(E_INVALID_PARAMS, "unknown controls: %s" % ", ".join(unknown), {"field": unknown[0]})

    def _start_encoder(self):
        from picamera2.encoders import H264Encoder
        self.session += 1
        self.seq = 0
        kw = dict(bitrate=self.enc_params["bitrate"], repeat=True, iperiod=self.enc_params["iperiod"])
        try:
            enc = H264Encoder(profile="high", **kw)
        except TypeError:  # older Picamera2 without the profile argument
            enc = H264Encoder(**kw)
        if self.output is None:
            self.output = _make_output(self)
        self.video.send_json(T_STREAM_START, {
            "session": self.session, "codec": "h264",
            "width": self.lores_size[0], "height": self.lores_size[1],
            "fps": self.fps, "profile": "high",
            "bitrate": self.enc_params["bitrate"], "iperiod": self.enc_params["iperiod"],
        })
        self.picam2.start_encoder(enc, self.output, name="lores")
        self.encoder = enc

    def _stop_encoder(self, reason):
        if self.encoder is not None:
            self.picam2.stop_encoder(self.encoder)
            self.encoder = None
            self.video.send_json(T_STREAM_END, {"session": self.session, "reason": reason})

    def _stop_stream(self, reason):
        self._stop_encoder(reason)
        self.picam2.stop()
        self.streaming = False

    def on_frame(self, frame, keyframe, timestamp):
        """Called from the encoder thread for every encoded frame."""
        if self.video_failed:
            return
        data = bytes(frame)
        flags = F_KEY if keyframe else 0
        if keyframe and has_sps(data):
            flags |= F_CONFIG
        try:
            self.video.send(T_AU, flags, self.seq, int(timestamp or 0), data)
        except OSError as e:
            # The server is gone: stop reading ctl so main() exits.
            self.video_failed = True
            log.error("video socket write failed: %s", e)
            try:
                self.ctl.sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            return
        self.seq += 1
        self.frames += 1

    def _post_callback(self, request):
        """Camera thread: sample metadata at metadata_hz."""
        if self.metadata_hz <= 0:
            return
        now = time.monotonic()
        if now - self._last_md < 1.0 / self.metadata_hz:
            return
        self._last_md = now
        try:
            md = request.get_metadata()
            self.ctl.notify("metadata", {k: md[k] for k in METADATA_KEYS if k in md})
        except Exception as e:  # noqa: BLE001
            log.warning("metadata: %s", e)


class _Release:
    def __init__(self, lock):
        self.lock = lock

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.lock.release()
        return False


def _make_output(cam):
    from picamera2.outputs import Output

    class SocketOutput(Output):
        def outputframe(self, frame, keyframe=True, timestamp=None, *args, **kwargs):
            cam.on_frame(frame, keyframe, timestamp)

    return SocketOutput()


def _stream_info(s):
    if not s:
        return None
    out = {}
    for k in ("size", "output_size", "format", "stride", "bit_depth"):
        if k in s and s[k] is not None:
            out["size" if k == "output_size" else k] = s[k] if not isinstance(s[k], tuple) else list(s[k])
    return out


def _enum_values(name):
    """libcamera enum names for a control, e.g. AwbMode -> {"Auto": 0, ...}."""
    try:
        from libcamera import controls as lc
    except Exception:  # noqa: BLE001
        return None
    for ns in (lc, getattr(lc, "draft", None)):
        enum = getattr(ns, name + "Enum", None) if ns is not None else None
        members = getattr(enum, "__members__", None)
        if members:
            try:
                return {k: int(v) for k, v in members.items()}
            except (TypeError, ValueError):
                return None
    return None


# ---------------------------------------------------------------- RPC

class Dispatcher:
    # Camera methods run in a worker thread so that quick ones (set_controls)
    # are never blocked behind them. Responses may therefore be out of order.
    CAMERA_METHODS = {"configure", "snapshot", "set_encoder", "stop"}

    def __init__(self, cam, ctl):
        self.cam, self.ctl = cam, ctl

    def handle(self, line):
        """Process one line. Returns False when the helper should exit."""
        try:
            msg = json.loads(line)
        except ValueError as e:
            self.ctl.reply_error(None, E_PARSE, "parse error: %s" % e)
            return True
        if not isinstance(msg, dict) or msg.get("jsonrpc") != "2.0" or "method" not in msg:
            self.ctl.reply_error(msg.get("id") if isinstance(msg, dict) else None,
                                 E_INVALID_REQUEST, "invalid request")
            return True
        id_, method, params = msg.get("id"), msg["method"], msg.get("params") or {}
        if id_ is None:
            log.warning("ignoring notification %s", method)
            return True
        if method == "shutdown":
            self.ctl.reply(id_, {})
            return False
        if method == "set_controls":
            self._run(id_, self.cam.set_controls, params)
        elif method in self.CAMERA_METHODS:
            fn = {"configure": self._locked(self.cam.configure),
                  "set_encoder": self._locked(self.cam.set_encoder),
                  "stop": self._locked(self.cam.stop),
                  "snapshot": self.cam.snapshot}[method]
            threading.Thread(target=self._run, args=(id_, fn, params), daemon=True,
                             name="rpc-%s-%s" % (method, id_)).start()
        else:
            self.ctl.reply_error(id_, E_METHOD_NOT_FOUND, "unknown method %s" % method)
        return True

    def _locked(self, fn):
        def wrapper(params):
            with self.cam._camera_op():
                return fn(params)
        return wrapper

    def _run(self, id_, fn, params):
        try:
            result = fn(params)
        except RpcError as e:
            self.ctl.reply_error(id_, e.code, e.message, e.data)
            return
        except Exception as e:  # noqa: BLE001
            log.error("%s failed:\n%s", getattr(fn, "__name__", fn), traceback.format_exc())
            self.cam.state("error", str(e))
            self.ctl.reply_error(id_, E_CAMERA, str(e))
            return
        self.ctl.reply(id_, result)


def stats_loop(cam, ctl, stop):
    last = 0
    while not stop.wait(1.0):
        if cam.streaming:
            n = cam.frames
            ctl.notify("stats", {"fps": n - last, "frames": n})
            last = n
        else:
            last = cam.frames


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--ctl", required=True, help="control socket path")
    ap.add_argument("--video", required=True, help="video socket path")
    ap.add_argument("--log-level", default="info")
    args = ap.parse_args()
    logging.basicConfig(stream=sys.stderr, level=args.log_level.upper(),
                        format="%(levelname)s %(name)s: %(message)s")

    def on_term(signum, _frame):
        raise SystemExit(128 + signum)
    signal.signal(signal.SIGTERM, on_term)

    ctl_sock = connect(args.ctl)
    video_sock = connect(args.video)
    video_sock.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 1 << 20)
    ctl, video = Ctl(ctl_sock), Video(video_sock)

    cam = Camera(ctl, video)
    stop = threading.Event()
    try:
        ctl.notify("hello", cam.hello())
        cam.state("idle")
        threading.Thread(target=stats_loop, args=(cam, ctl, stop), daemon=True).start()
        disp = Dispatcher(cam, ctl)
        for line in ctl.lines():
            if not disp.handle(line):
                break
        log.info("exiting")
    finally:
        stop.set()
        cam.close()
        for s in (ctl_sock, video_sock):
            try:
                s.close()
            except OSError:
                pass


if __name__ == "__main__":
    main()
