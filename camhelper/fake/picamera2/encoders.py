"""Fake picamera2.encoders."""


class H264Encoder:
    def __init__(self, bitrate=None, repeat=False, iperiod=None, framerate=None,
                 enable_sps_framerate=False, qp=None, profile=None):
        self.bitrate, self.repeat, self.iperiod, self.profile = bitrate, repeat, iperiod, profile
        self.framerate = framerate
        self.output = None
        self.index = 0
        self.firsttimestamp = None


class MJPEGEncoder:
    def __init__(self, bitrate=None):
        self.bitrate = bitrate
        self.framerate = None  # inherited from V4L2Encoder in the real Picamera2
        self._enable_framerate = False
        self.frame_skip_count = 1
        self.output = None
        self.index = 0
        self.firsttimestamp = None
