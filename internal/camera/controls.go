package camera

import (
	"math"
	"reflect"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

// libcamera enum names for our values. The integer values come from the
// camera (hello.camera.controls[...].values).
var enumNames = map[string]map[string]string{
	"AeMeteringMode": {"centre": "CentreWeighted", "spot": "Spot", "matrix": "Matrix"},
	"AeExposureMode": {"normal": "Normal", "short": "Short", "long": "Long"},
	"AwbMode": {"auto": "Auto", "incandescent": "Incandescent", "tungsten": "Tungsten",
		"fluorescent": "Fluorescent", "indoor": "Indoor", "daylight": "Daylight", "cloudy": "Cloudy"},
	"NoiseReductionMode": {"off": "Off", "fast": "Fast", "high_quality": "HighQuality", "minimal": "Minimal"},
}

// enumValue returns the camera's integer value for one of our enum values,
// or false if the camera does not have it.
func enumValue(hello *proto.Hello, control, ours string) (int, bool) {
	name, ok := enumNames[control][ours]
	if !ok {
		return 0, false
	}
	v, ok := hello.Camera.Controls[control].Values[name]
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

// ControlMap converts Controls to libcamera controls for the camera described
// by hello (enum values and supported controls come from it). Controls the
// camera does not list are left out.
//
// Auto/manual exposure uses ExposureTimeMode and AnalogueGainMode
// (0 = auto, 1 = manual); manual values are sent only in manual mode.
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
	if c.AutoExposure {
		m["ExposureTimeMode"] = 0
		m["AnalogueGainMode"] = 0
	} else {
		m["ExposureTimeMode"] = 1
		m["AnalogueGainMode"] = 1
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
	// "auto" is Picamera2's video default, Fast. It is sent explicitly so that
	// switching back to "auto" from another mode takes effect.
	denoise := c.Denoise
	if denoise == "auto" {
		denoise = "fast"
	}
	if v, ok := enumValue(hello, "NoiseReductionMode", denoise); ok {
		m["NoiseReductionMode"] = v
	}
	return filterSupported(m, hello)
}

func filterSupported(m map[string]any, hello *proto.Hello) map[string]any {
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
