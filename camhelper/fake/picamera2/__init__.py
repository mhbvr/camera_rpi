"""Fake Picamera2 for running camhelper.py without a Raspberry Pi."""
import math
import os
import threading
import time

import numpy as np

__version__ = "0.0-fake"

_HERE = os.path.dirname(os.path.abspath(__file__))
_DEFAULT_H264 = os.path.join(_HERE, "..", "..", "..", "cmd", "fakecam", "testdata", "test.h264")


def _split_aus(data):
    """Split an Annex-B stream into access units (single-slice streams)."""
    starts = []
    i = data.find(b"\x00\x00\x01")
    while i >= 0:
        starts.append(i + 3)
        i = data.find(b"\x00\x00\x01", i + 3)
    nals = []
    for n, s in enumerate(starts):
        e = starts[n + 1] - 3 if n + 1 < len(starts) else len(data)
        if e > s and data[e - 1] == 0 and n + 1 < len(starts):
            e -= 1
        nals.append(data[s:e])
    aus, cur, saw_vcl = [], b"", False
    for nal in nals:
        t = nal[0] & 0x1F
        vcl = 1 <= t <= 5
        if t == 9 or (saw_vcl and (not vcl or (len(nal) > 1 and nal[1] & 0x80))):
            aus.append(cur)
            cur, saw_vcl = b"", False
        cur += b"\x00\x00\x00\x01" + nal
        saw_vcl = saw_vcl or vcl
    if cur:
        aus.append(cur)
    return aus


class _Request:
    def __init__(self, cam, frame):
        self.cam, self.frame = cam, frame

    def get_metadata(self):
        c = self.cam._controls
        t = time.monotonic()
        auto = c.get("AeEnable", True) and c.get("ExposureTimeMode", 0) == 0
        fd = int(1e6 / self.cam._fps())
        if auto and os.environ.get("FAKE_DARK"):  # auto exposure stuck at its limits
            exp, gain = min(fd - 300, 66666), 16.0
        elif auto:
            exp, gain = 8000, 2.0
        else:
            exp, gain = c.get("ExposureTime", 8000), c.get("AnalogueGain", 2.0)
        return {
            "ExposureTime": exp,
            "AnalogueGain": gain,
            "AeState": 2 if auto else 0,
            "DigitalGain": 1.0,
            "ColourGains": tuple(c.get("ColourGains", (1.8, 1.5))),
            "ColourTemperature": 4800 + 100 * math.sin(t / 5),
            "Lux": 300.0,
            "FrameDuration": int(1e6 / self.cam._fps()),
            "FocusFoM": int(1500 + 1000 * math.sin(t / 2)),
            "SensorTimestamp": time.monotonic_ns(),
        }

    def make_array(self, name):
        s = self.cam._cfg[name]
        w, h = s["size"]
        stride = s["stride"]
        arr = np.full((h * 3 // 2, stride), 128, dtype=np.uint8)
        x = np.arange(w, dtype=np.uint16)
        y = np.arange(h, dtype=np.uint16)[:, None]
        arr[:h, :w] = ((x + y + self.frame * 4) % 220 + 16).astype(np.uint8)
        return arr

    def release(self):
        pass


class Picamera2:
    def __init__(self, camera_num=0):
        with open(os.environ.get("FAKE_H264", _DEFAULT_H264), "rb") as f:
            self._aus = _split_aus(f.read())
        self.camera_properties = {"Model": "imx477", "PixelArraySize": (4056, 3040), "ScalerCropMaximum": (0, 0, 4056, 3040)}
        self.camera_controls = {
            "AeEnable": (False, True, None), "ExposureTime": (31, 667234896, None),
            "AnalogueGain": (1.0, 22.2608699798584, None), "ExposureValue": (-8.0, 8.0, 0.0),
            "AeMeteringMode": (0, 3, 0), "AeExposureMode": (0, 3, 0), "AwbEnable": (False, True, None),
            "AwbMode": (0, 7, 0), "ColourGains": (0.0, 32.0, None), "Brightness": (-1.0, 1.0, 0.0),
            "Contrast": (0.0, 32.0, 1.0), "Saturation": (0.0, 32.0, 1.0), "Sharpness": (0.0, 16.0, 1.0),
            "NoiseReductionMode": (0, 4, 0), "FrameDurationLimits": (8333, 667244877, None),
            "ScalerCrop": ((0, 0, 64, 64), (0, 0, 4056, 3040), (2, 0, 4052, 3040)),
        }
        self.post_callback = None
        self._cfg = None
        self._controls = {}
        self._encoders = []
        self._running = False
        self._thread = None
        self._frame = 0
        self._cond = threading.Condition()

    @property
    def sensor_modes(self):
        return [
            {"format": "SRGGB10_CSI2P", "size": (1332, 990), "fps": 120.05, "bit_depth": 10,
             "crop_limits": (696, 528, 2664, 1980), "exposure_limits": (31, 667234896, None)},
            {"format": "SRGGB12_CSI2P", "size": (2028, 1080), "fps": 50.03, "bit_depth": 12,
             "crop_limits": (0, 440, 4056, 2160), "exposure_limits": (60, 674181621, None)},
            {"format": "SRGGB12_CSI2P", "size": (2028, 1520), "fps": 40.01, "bit_depth": 12,
             "crop_limits": (0, 0, 4056, 3040), "exposure_limits": (60, 674181621, None)},
            {"format": "SRGGB12_CSI2P", "size": (4056, 3040), "fps": 10.0, "bit_depth": 12,
             "crop_limits": (0, 0, 4056, 3040), "exposure_limits": (114, 694422939, None)},
        ]

    def create_video_configuration(self, main={}, lores=None, raw={}, transform=None, colour_space=None,
                                   buffer_count=6, controls={}, display="main", encode="main", queue=True,
                                   sensor={}, use_case="video"):
        return {"main": dict(main), "lores": dict(lores) if lores else None, "sensor": dict(sensor),
                "transform": transform, "buffer_count": buffer_count, "controls": dict(controls),
                "colour_space": "Rec709", "display": display, "encode": encode}

    def configure(self, cfg):
        if self._running:
            raise RuntimeError("Camera must be stopped before configuring")
        for name in ("main", "lores"):
            s = cfg.get(name)
            if s:
                w = s["size"][0]
                s["stride"] = (w + 63) // 64 * 64
        lw, lh = cfg["lores"]["size"]
        mw, mh = cfg["main"]["size"]
        if lw > mw or lh > mh:
            raise RuntimeError("lores stream dimensions may not exceed main stream")
        self._check(cfg["controls"])
        self._controls.update(cfg["controls"])
        self._cfg = cfg

    def camera_configuration(self):
        return self._cfg

    def _check(self, controls):
        for k in controls:
            if k not in self.camera_controls:
                raise RuntimeError("Control %s is not advertised by libcamera" % k)

    def set_controls(self, controls):
        self._check(controls)
        self._controls.update(controls)

    def _fps(self):
        fdl = self._controls.get("FrameDurationLimits")
        return 1e6 / fdl[0] if fdl else 30.0

    def start(self):
        if self._cfg is None:
            raise RuntimeError("Camera has not been configured")
        self._running = True
        self._thread = threading.Thread(target=self._loop, daemon=True)
        self._thread.start()

    def stop(self):
        self._running = False
        if self._thread:
            self._thread.join()
            self._thread = None

    def start_encoder(self, encoder, output, name="main"):
        if self._cfg is None:
            raise RuntimeError("Camera has not been configured")
        encoder.output, encoder.index = output, 0
        output.start()
        self._encoders.append(encoder)

    def stop_encoder(self, encoders=None):
        encs = self._encoders if encoders is None else (encoders if isinstance(encoders, list) else [encoders])
        for e in list(encs):
            if e in self._encoders:
                self._encoders.remove(e)
                e.output.stop()

    def capture_request(self):
        with self._cond:
            n = self._frame
            if not self._cond.wait_for(lambda: self._frame > n, timeout=5):
                raise RuntimeError("timed out waiting for a frame")
            return _Request(self, self._frame)

    def close(self):
        self.stop()

    def _loop(self):
        next_t = time.monotonic()
        while self._running:
            next_t += 1.0 / self._fps()
            time.sleep(max(0, next_t - time.monotonic()))
            with self._cond:
                self._frame += 1
                self._cond.notify_all()
            req = _Request(self, self._frame)
            if self.post_callback:
                self.post_callback(req)
            for enc in list(self._encoders):
                au = self._aus[enc.index % len(self._aus)]
                enc.index += 1
                key = b"\x00\x00\x01\x65" in au or b"\x00\x00\x01\x25" in au or b"\x00\x00\x01\x45" in au
                # Like Picamera2: timestamp relative to the first frame, and
                # firsttimestamp = that frame's SensorTimestamp in µs.
                ts = time.monotonic_ns() // 1000 - 30000  # pretend 30 ms capture latency
                if enc.firsttimestamp is None:
                    enc.firsttimestamp = ts
                enc.output.outputframe(au, key, ts - enc.firsttimestamp, None, False)
