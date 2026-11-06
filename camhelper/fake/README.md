# Fake Picamera2 / libcamera

A minimal stand-in for `picamera2` and `libcamera` so `camhelper.py` can run
on a development machine without a Raspberry Pi. It implements only the API
surface camhelper uses. The "encoder" replays `cmd/fakecam/testdata/test.h264`
(or `$FAKE_H264`) in a loop at the configured frame rate.

    PYTHONPATH=camhelper/fake ./bin/camweb -helper-cmd "python3 camhelper/camhelper.py"

Needs numpy and Pillow (or simplejpeg).
