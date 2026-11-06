"""Fake libcamera (only what camhelper uses)."""
__version__ = "fake"


class Transform:
    def __init__(self, hflip=0, vflip=0, transpose=0):
        self.hflip, self.vflip, self.transpose = int(hflip), int(vflip), int(transpose)

    def __repr__(self):
        return "<Transform hflip=%d vflip=%d>" % (self.hflip, self.vflip)


from . import controls  # noqa: E402,F401
