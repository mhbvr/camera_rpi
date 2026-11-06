"""Fake picamera2.outputs."""


class Output:
    def __init__(self, pts=None):
        self.recording = False

    def start(self):
        self.recording = True

    def stop(self):
        self.recording = False

    def outputframe(self, frame, keyframe=True, timestamp=None, packet=None, audio=False):
        pass
