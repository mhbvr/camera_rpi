"""Fake picamera2.encoders."""


class H264Encoder:
    def __init__(self, bitrate=None, repeat=False, iperiod=None, framerate=None,
                 enable_sps_framerate=False, qp=None, profile=None):
        self.bitrate, self.repeat, self.iperiod, self.profile = bitrate, repeat, iperiod, profile
        self.output = None
        self.index = 0
