"""Fake libcamera.controls enums."""
from enum import IntEnum

AeMeteringModeEnum = IntEnum("AeMeteringModeEnum", {"CentreWeighted": 0, "Spot": 1, "Matrix": 2, "Custom": 3})
AeExposureModeEnum = IntEnum("AeExposureModeEnum", {"Normal": 0, "Short": 1, "Long": 2, "Custom": 3})
AwbModeEnum = IntEnum("AwbModeEnum", {"Auto": 0, "Incandescent": 1, "Tungsten": 2, "Fluorescent": 3,
                                      "Indoor": 4, "Daylight": 5, "Cloudy": 6, "Custom": 7})


class draft:
    NoiseReductionModeEnum = IntEnum("NoiseReductionModeEnum", {"Off": 0, "Fast": 1, "HighQuality": 2, "Minimal": 3, "ZSL": 4})
