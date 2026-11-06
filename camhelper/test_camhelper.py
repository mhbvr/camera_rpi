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

    def test_encoder_bitrate(self):
        self.assertEqual(camhelper.encoder_bitrate(6_000_000, 30), 6_000_000)
        self.assertEqual(camhelper.encoder_bitrate(6_000_000, 40), 4_500_000)
        self.assertEqual(camhelper.encoder_bitrate(6_000_000, 15), 12_000_000)
        self.assertEqual(camhelper.encoder_bitrate(20_000_000, 10), camhelper.MAX_ENCODER_BITRATE)

    def test_has_sps(self):
        self.assertTrue(camhelper.has_sps(b"\x00\x00\x00\x01\x67\x64\x00\x28\x00\x00\x01\x68"))
        self.assertFalse(camhelper.has_sps(b"\x00\x00\x00\x01\x65\x88\x84"))


class TuningTest(unittest.TestCase):
    def test_read_tuning_from_fake_picamera2(self):
        fake = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fake")
        sys.path.insert(0, fake)
        try:
            t = camhelper.read_tuning("imx477")
        finally:
            sys.path.remove(fake)
        if t is None:
            self.skipTest("fake picamera2 not importable (numpy missing?)")
        self.assertEqual(t["exposure_modes"]["long"]["shutter"][-1], 120000)
        self.assertEqual(t["exposure_modes"]["normal"]["gain"][-1], 16.0)
        self.assertEqual(t["max_digital_gain"], 4.0)
        self.assertEqual(t["metering_modes"]["spot"][:3], [2, 1, 1])
        self.assertEqual(len(t["metering_modes"]["matrix"]), 15)
        self.assertEqual(t["awb"]["modes"]["daylight"], [5500, 6500])
        self.assertEqual(t["awb"]["ct_curve"][0], [2360.0, 0.6009, 0.3093])
        self.assertEqual(t["awb"]["sensitivity_r"], 1.05)
        self.assertIsNone(camhelper.read_tuning(""))


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

    def test_inline_and_shutdown(self):
        self.d.handle(json.dumps({"jsonrpc": "2.0", "id": 1, "method": "set_controls", "params": {"controls": {}}}).encode())
        self.assertEqual(self.ctl.sent[-1], {"id": 1, "result": {}})
        self.assertFalse(self.d.handle(json.dumps({"jsonrpc": "2.0", "id": 2, "method": "shutdown"}).encode()))


if __name__ == "__main__":
    unittest.main()
