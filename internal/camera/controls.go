package camera

import (
	"math"
	"reflect"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

// libcamera enum names for our values, and their standard integer values
// (used when the helper does not report enum names).
var enumNames = map[string]map[string]string{
	"AeMeteringMode": {"centre": "CentreWeighted", "spot": "Spot", "matrix": "Matrix"},
	"AeExposureMode": {"normal": "Normal", "short": "Short", "long": "Long"},
	"AwbMode": {"auto": "Auto", "incandescent": "Incandescent", "tungsten": "Tungsten",
		"fluorescent": "Fluorescent", "indoor": "Indoor", "daylight": "Daylight", "cloudy": "Cloudy"},
	"NoiseReductionMode": {"off": "Off", "fast": "Fast", "high_quality": "HighQuality", "minimal": "Minimal"},
}

var enumDefaults = map[string]map[string]int{
	"AeMeteringMode":     {"CentreWeighted": 0, "Spot": 1, "Matrix": 2, "Custom": 3},
	"AeExposureMode":     {"Normal": 0, "Short": 1, "Long": 2, "Custom": 3},
	"AwbMode":            {"Auto": 0, "Incandescent": 1, "Tungsten": 2, "Fluorescent": 3, "Indoor": 4, "Daylight": 5, "Cloudy": 6, "Custom": 7},
	"NoiseReductionMode": {"Off": 0, "Fast": 1, "HighQuality": 2, "Minimal": 3, "ZSL": 4},
}

func enumValue(hello *proto.Hello, control, ours string) (int, bool) {
	name, ok := enumNames[control][ours]
	if !ok {
		return 0, false
	}
	if hello != nil {
		if ci, ok := hello.Camera.Controls[control]; ok && ci.Values != nil {
			v, ok := ci.Values[name]
			return v, ok
		}
	}
	v, ok := enumDefaults[control][name]
	return v, ok
}

// FrameDurationUS returns the frame duration for the controls. With manual
// exposure longer than the frame time, the frame time is stretched so the
// exposure fits.
func FrameDurationUS(c Controls) int {
	d := int(math.Round(1e6 / c.FPS))
	if !c.AutoExposure && c.ShutterUS > d {
		d = c.ShutterUS
	}
	return d
}

// EncoderFrameRate is the frame rate passed to the encoder's rate control.
func EncoderFrameRate(c Controls) float64 {
	return math.Round(1e9/float64(FrameDurationUS(c))) / 1000
}

// ControlMap converts Controls to libcamera controls. When hello reports a
// control list, controls the camera does not support are left out.
//
// Auto/manual exposure: newer libcamera has ExposureTimeMode and
// AnalogueGainMode (0 = auto, 1 = manual); older Raspberry Pi libcamera
// treats ExposureTime = 0 / AnalogueGain = 0 as "automatic". Both are
// handled; which one applies is checked in the M0 spike.
func ControlMap(c Controls, hello *proto.Hello) map[string]any {
	d := FrameDurationUS(c)
	m := map[string]any{
		"FrameDurationLimits": []int{d, d},
		"AeEnable":            c.AutoExposure,
		"ExposureValue":       c.EV,
		"AwbEnable":           c.AutoWB,
		"Brightness":          c.Brightness,
		"Contrast":            c.Contrast,
		"Saturation":          c.Saturation,
		"Sharpness":           c.Sharpness,
	}
	if v, ok := enumValue(hello, "AeMeteringMode", c.Metering); ok {
		m["AeMeteringMode"] = v
	}
	if v, ok := enumValue(hello, "AeExposureMode", c.ExposureMode); ok {
		m["AeExposureMode"] = v
	}
	hasModes := supports(hello, "ExposureTimeMode")
	if c.AutoExposure {
		if hasModes {
			m["ExposureTimeMode"] = 0
			m["AnalogueGainMode"] = 0
		} else {
			m["ExposureTime"] = 0
			m["AnalogueGain"] = 0.0
		}
	} else {
		if hasModes {
			m["ExposureTimeMode"] = 1
			m["AnalogueGainMode"] = 1
		}
		m["ExposureTime"] = c.ShutterUS
		m["AnalogueGain"] = c.Gain
	}
	if c.AutoWB {
		if v, ok := enumValue(hello, "AwbMode", c.AWBMode); ok {
			m["AwbMode"] = v
		}
	} else {
		m["ColourGains"] = []float64{c.WBGainR, c.WBGainB}
	}
	if c.Denoise != "auto" {
		if v, ok := enumValue(hello, "NoiseReductionMode", c.Denoise); ok {
			m["NoiseReductionMode"] = v
		}
	}
	return filterSupported(m, hello)
}

func supports(hello *proto.Hello, name string) bool {
	if hello == nil {
		return false
	}
	_, ok := hello.Camera.Controls[name]
	return ok
}

func filterSupported(m map[string]any, hello *proto.Hello) map[string]any {
	if hello == nil || len(hello.Camera.Controls) == 0 {
		return m
	}
	for k := range m {
		if _, ok := hello.Camera.Controls[k]; !ok {
			delete(m, k)
		}
	}
	return m
}

// ChangedControls returns the entries of new that differ from old.
func ChangedControls(old, new map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range new {
		if ov, ok := old[k]; !ok || !reflect.DeepEqual(ov, v) {
			out[k] = v
		}
	}
	return out
}

// ConfigureParams builds the "configure" params for c.
func ConfigureParams(c Config, hello *proto.Hello, hflip, vflip bool) proto.ConfigureParams {
	return proto.ConfigureParams{
		Sensor:      proto.SensorCfg{Size: SensorSize, BitDepth: SensorBitDepth},
		Main:        proto.StreamCfg{Size: MainSize, Format: "YUV420"},
		Lores:       proto.StreamCfg{Size: c.Pipeline.StreamSize, Format: "YUV420"},
		Transform:   proto.Transform{HFlip: hflip, VFlip: vflip},
		BufferCount: c.Pipeline.BufferCount,
		Controls:    ControlMap(c.Controls, hello),
		Encoder:     proto.EncoderCfg{Codec: "h264", Bitrate: c.Encoder.Bitrate, IPeriod: c.Encoder.GOP, FrameRate: EncoderFrameRate(c.Controls)},
		MetadataHz:  2,
	}
}
