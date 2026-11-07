"""Unit tests for camhelper.py that run without a camera.

    python3 -m unittest discover -s camhelper -p 'test_*.py'
"""
import io
import json
import os
import struct
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import camhelper  # noqa: E402

try:
    import numpy as np
except ImportError:  # pragma: no cover
    np = None


def make_yuv420(rgb, stride):
    """RGB (h, w, 3) -> Picamera2-style YUV420 array (h*3/2, stride),
    limited range BT.601, with row padding."""
    h, w, _ = rgb.shape
    r, g, b = [rgb[..., i].astype(np.float32) for i in range(3)]
    y = 16 + (65.481 * r + 128.553 * g + 24.966 * b) / 255
    cb = 128 + (-37.797 * r - 74.203 * g + 112.0 * b) / 255
    cr = 128 + (112.0 * r - 93.786 * g - 18.214 * b) / 255
    sub = lambda p: p.reshape(h // 2, 2, w // 2, 2).mean(axis=(1, 3))  # noqa: E731
    flat = np.full(h * 3 // 2 * stride, 99, dtype=np.uint8)  # 99 = padding garbage
    ys = flat[:h * stride].reshape(h, stride)
    ys[:, :w] = np.clip(y, 0, 255).round()
    cs = stride // 2
    base = h * stride
    for i, p in enumerate((sub(cb), sub(cr))):
        plane = flat[base + i * (h // 2) * cs: base + (i + 1) * (h // 2) * cs].reshape(h // 2, cs)
        plane[:, :w // 2] = np.clip(p, 0, 255).round()
    return flat.reshape(h * 3 // 2, stride)


@unittest.skipIf(np is None, "numpy not installed")
class JpegTest(unittest.TestCase):
    def roundtrip(self):
        from PIL import Image
        h, w, stride = 120, 200, 256
        rgb = np.zeros((h, w, 3), dtype=np.uint8)
        rgb[:, :w // 2] = (200, 40, 40)   # red-ish left half
        rgb[:, w // 2:] = (30, 160, 220)  # blue-ish right half
        arr = make_yuv420(rgb, stride)
        jpeg = camhelper.encode_yuv420_jpeg(arr, w, h, 95, full_range=False)
        out = np.asarray(Image.open(io.BytesIO(jpeg)).convert("RGB")).astype(int)
        self.assertEqual(out.shape, (h, w, 3))
        left = out[10:-10, 10:w // 2 - 10].reshape(-1, 3).mean(axis=0)
        right = out[10:-10, w // 2 + 10:-10].reshape(-1, 3).mean(axis=0)
        np.testing.assert_allclose(left, (200, 40, 40), atol=8)
        np.testing.assert_allclose(right, (30, 160, 220), atol=8)

    def grey_ramp(self):
        """Grey patches over the whole range must keep their brightness (the
        old int16 code turned everything above Y≈144 black)."""
        from PIL import Image
        levels = [10, 60, 120, 150, 180, 210, 240, 252]
        h, pw, stride = 64, 32, 320
        w = pw * len(levels)
        rgb = np.zeros((h, w, 3), dtype=np.uint8)
        for i, lv in enumerate(levels):
            rgb[:, i * pw:(i + 1) * pw] = lv
        arr = make_yuv420(rgb, stride)
        jpeg = camhelper.encode_yuv420_jpeg(arr, w, h, 95, full_range=False)
        out = np.asarray(Image.open(io.BytesIO(jpeg)).convert("L")).astype(int)
        got = [out[8:-8, i * pw + 8:(i + 1) * pw - 8].mean() for i in range(len(levels))]
        for lv, g in zip(levels, got):
            self.assertAlmostEqual(g, lv, delta=6, msg="grey %d came out as %.1f" % (lv, g))

    def test_grey_ramp_pillow(self):
        saved = sys.modules.get("simplejpeg")
        sys.modules["simplejpeg"] = None
        try:
            self.grey_ramp()
        finally:
            if saved is None:
                del sys.modules["simplejpeg"]
            else:
                sys.modules["simplejpeg"] = saved

    def test_grey_ramp_simplejpeg(self):
        try:
            import simplejpeg  # noqa: F401
        except ImportError:
            self.skipTest("simplejpeg not installed")
        self.grey_ramp()

    def test_fallback_pillow(self):
        saved = sys.modules.get("simplejpeg")
        sys.modules["simplejpeg"] = None  # force ImportError
        try:
            self.roundtrip()
        finally:
            if saved is None:
                del sys.modules["simplejpeg"]
            else:
                sys.modules["simplejpeg"] = saved

    def test_simplejpeg(self):
        try:
            import simplejpeg  # noqa: F401
        except ImportError:
            self.skipTest("simplejpeg not installed")
        self.roundtrip()


class FramingTest(unittest.TestCase):
    def test_header(self):
        self.assertEqual(camhelper.HEADER.size, 32)
        hdr = camhelper.HEADER.pack(5, 1, 3, 0, 7, 8, 9)
        self.assertEqual(struct.unpack(">I", hdr[:4])[0], 5)
        self.assertEqual(hdr[4], 1)
        self.assertEqual(hdr[5], 3)

    def test_make_h264_encoder(self):
        class V4L2:  # Pi 4 hardware encoder
            def __init__(self, bitrate=None, repeat=True, iperiod=None, framerate=None,
                         enable_sps_framerate=False, qp=None, profile=None):
                self.kw = dict(bitrate=bitrate, framerate=framerate, sps=enable_sps_framerate, profile=profile)

        class Libav:  # Pi 5 software encoder: framerate, but no enable_sps_framerate
            def __init__(self, bitrate=None, repeat=True, iperiod=30, framerate=30, qp=None, profile=None):
                self.kw = dict(bitrate=bitrate, framerate=framerate, profile=profile)

        class Old:  # Picamera2 before 0.3.17
            def __init__(self, bitrate=None, repeat=True, iperiod=None, framerate=None, enable_sps_framerate=False):
                pass

        enc = camhelper.make_h264_encoder(V4L2, 6_000_000, 40, 40.0)
        self.assertEqual(enc.kw, dict(bitrate=6_000_000, framerate=40.0, sps=True, profile="high"))
        enc = camhelper.make_h264_encoder(Libav, 6_000_000, 40, 40.0)
        self.assertEqual(enc.kw, dict(bitrate=6_000_000, framerate=40.0, profile="high"))
        with self.assertRaises(camhelper.RpcError):
            camhelper.make_h264_encoder(Old, 6_000_000, 40, 40.0)

    def test_make_mjpeg_encoder(self):
        class V4L2:  # Pi 4: inherits V4L2Encoder's frame-rate attributes
            def __init__(self, bitrate=None):
                self.bitrate, self.framerate, self._enable_framerate = bitrate, None, False
                self.frame_skip_count = 1

        class Libav:  # Pi 5: framerate argument
            def __init__(self, bitrate=None, repeat=True, iperiod=30, framerate=30, qp=None):
                self.bitrate, self.framerate, self.frame_skip_count = bitrate, framerate, 1

        class Old:
            def __init__(self, bitrate=None):
                self.bitrate = bitrate

        enc = camhelper.make_mjpeg_encoder(V4L2, 8_000_000, 30.0, 2)
        self.assertEqual((enc.bitrate, enc.framerate, enc._enable_framerate, enc.frame_skip_count),
                         (8_000_000, 15.0, True, 2))
        enc = camhelper.make_mjpeg_encoder(Libav, 8_000_000, 30.0, 3)
        self.assertEqual((enc.bitrate, enc.framerate, enc.frame_skip_count), (8_000_000, 10.0, 3))
        with self.assertRaises(camhelper.RpcError):
            camhelper.make_mjpeg_encoder(Old, 8_000_000, 30.0, 2)

    def test_has_sps(self):
        self.assertTrue(camhelper.has_sps(b"\x00\x00\x00\x01\x67\x64\x00\x28\x00\x00\x01\x68"))
        self.assertFalse(camhelper.has_sps(b"\x00\x00\x00\x01\x65\x88\x84"))


class FakeCtl:
    def __init__(self):
        self.sent = []

    def reply(self, id_, result):
        self.sent.append({"id": id_, "result": result})

    def reply_error(self, id_, code, message, data=None):
        self.sent.append({"id": id_, "error": {"code": code, "message": message}})


class DispatcherTest(unittest.TestCase):
    def setUp(self):
        self.ctl = FakeCtl()
        self.cam = type("C", (), {"set_controls": lambda s, p: {}})()
        self.d = camhelper.Dispatcher(self.cam, self.ctl)

    def test_errors(self):
        self.assertTrue(self.d.handle(b"not json"))
        self.assertEqual(self.ctl.sent[-1]["error"]["code"], camhelper.E_PARSE)
        self.d.handle(json.dumps([1, 2]).encode())
        self.assertEqual(self.ctl.sent[-1]["error"]["code"], camhelper.E_INVALID_REQUEST)
        self.d.handle(json.dumps({"jsonrpc": "2.0", "id": 3, "method": "nope"}).encode())
        self.assertEqual(self.ctl.sent[-1], {"id": 3, "error": {"code": camhelper.E_METHOD_NOT_FOUND, "message": "unknown method nope"}})

    def test_routes_and_lock(self):
        calls = []

        class Cam:
            camera_op = camhelper.Camera.camera_op  # the real lock handling

            def state(self_, *a):
                pass

            def configure(self_, p):
                calls.append(("configure", self_.lock.locked()))
                return {"session": 1}

        cam = Cam()
        cam.lock = camhelper.threading.Lock()
        d = camhelper.Dispatcher(cam, self.ctl)
        d.handle(json.dumps({"jsonrpc": "2.0", "id": 5, "method": "configure", "params": {}}).encode())
        for _ in range(100):
            if self.ctl.sent and self.ctl.sent[-1]["id"] == 5:
                break
            camhelper.time.sleep(0.01)
        self.assertEqual(self.ctl.sent[-1], {"id": 5, "result": {"session": 1}})
        self.assertEqual(calls, [("configure", True)])  # ran with the camera lock held
        cam.lock.acquire()  # a second camera operation while one runs -> busy
        d.handle(json.dumps({"jsonrpc": "2.0", "id": 6, "method": "configure", "params": {}}).encode())
        for _ in range(100):
            if self.ctl.sent[-1]["id"] == 6:
                break
            camhelper.time.sleep(0.01)
        self.assertEqual(self.ctl.sent[-1]["error"]["code"], camhelper.E_BUSY)
        cam.lock.release()

    def test_inline_and_shutdown(self):
        self.d.handle(json.dumps({"jsonrpc": "2.0", "id": 1, "method": "set_controls", "params": {"controls": {}}}).encode())
        self.assertEqual(self.ctl.sent[-1], {"id": 1, "result": {}})
        self.assertFalse(self.d.handle(json.dumps({"jsonrpc": "2.0", "id": 2, "method": "shutdown"}).encode()))


if __name__ == "__main__":
    unittest.main()
