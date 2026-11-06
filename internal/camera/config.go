// Package camera holds the user-facing camera configuration, its mapping to
// libcamera controls, and the Manager that drives the helper.
package camera

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

// Fixed pipeline geometry (DESIGN.md §3.1): one sensor mode, 2×2 binned,
// full field of view. To be confirmed by the M0 spike.
var (
	SensorSize     = proto.Size{2028, 1520}
	SensorBitDepth = 12
	MainSize       = proto.Size{2028, 1520}
	StreamSizes    = []proto.Size{{1440, 1080}, {1024, 768}, {640, 480}}
)

// Limits are the Go-side validation ranges. They are also rendered into the
// settings form.
var Limits = struct {
	FPSMin, FPSMax               float64
	BitrateMin, BitrateMax       int
	GOPMin, GOPMax               int
	BufferMin, BufferMax         int
	ShutterMin, ShutterMax       int
	GainMin, GainMax             float64
	EVMin, EVMax                 float64
	BrightnessMin, BrightnessMax float64
	ContrastMax, SaturationMax   float64
	SharpnessMax                 float64
	WBGainMin, WBGainMax         float64
	QualityMin, QualityMax       int
	MJPEGBitrateMin              int
	MJPEGBitrateMax              int
	MJPEGSkipMax                 int
}{
	FPSMin: 1, FPSMax: 40,
	BitrateMin: 250_000, BitrateMax: 25_000_000,
	GOPMin: 1, GOPMax: 300,
	BufferMin: 2, BufferMax: 12,
	ShutterMin: 31, ShutterMax: 1_000_000,
	GainMin: 1, GainMax: 22,
	EVMin: -8, EVMax: 8,
	BrightnessMin: -1, BrightnessMax: 1,
	ContrastMax: 32, SaturationMax: 32,
	SharpnessMax: 16,
	WBGainMin:    0.1, WBGainMax: 32,
	QualityMin: 1, QualityMax: 100,
	MJPEGBitrateMin: 1_000_000, MJPEGBitrateMax: 40_000_000,
	MJPEGSkipMax: 10,
}

// Allowed enum values (our names).
var (
	MeteringModes = []string{"centre", "spot", "matrix"}
	ExposureModes = []string{"normal", "short", "long"}
	AWBModes      = []string{"auto", "incandescent", "tungsten", "fluorescent", "indoor", "daylight", "cloudy"}
	DenoiseModes  = []string{"auto", "off", "fast", "high_quality", "minimal"}
)

// Config is the complete user-facing configuration, split by how disruptive
// a change is.
type Config struct {
	Pipeline Pipeline `json:"pipeline"` // needs reconfigure
	Encoder  Encoder  `json:"encoder"`  // needs encoder restart
	Controls Controls `json:"controls"` // live
	Still    Still    `json:"still"`    // per capture
	MJPEG    MJPEG    `json:"mjpeg"`    // fallback encoder, only while MJPEG viewers watch
}

// MJPEG configures the fallback stream for browsers without WebCodecs.
type MJPEG struct {
	Bitrate int `json:"bitrate"` // bps requested from the hardware MJPEG encoder
	Skip    int `json:"skip"`    // encode every Skip-th camera frame (2 = half the frame rate)
}

// Pipeline settings require reconfiguring the camera.
type Pipeline struct {
	StreamSize  proto.Size `json:"stream_size"`
	BufferCount int        `json:"buffer_count"`
}

// Encoder settings require restarting the encoder.
type Encoder struct {
	Bitrate int `json:"bitrate"`
	GOP     int `json:"gop"`
}

// Controls are applied live with set_controls.
type Controls struct {
	FPS          float64 `json:"fps"`
	AutoExposure bool    `json:"auto_exposure"`
	ShutterUS    int     `json:"shutter_us"`
	Gain         float64 `json:"gain"`
	EV           float64 `json:"ev"`
	Metering     string  `json:"metering"`
	ExposureMode string  `json:"exposure_mode"`
	AutoWB       bool    `json:"auto_wb"`
	AWBMode      string  `json:"awb_mode"`
	WBGainR      float64 `json:"wb_gain_r"`
	WBGainB      float64 `json:"wb_gain_b"`
	Brightness   float64 `json:"brightness"`
	Contrast     float64 `json:"contrast"`
	Saturation   float64 `json:"saturation"`
	Sharpness    float64 `json:"sharpness"`
	Denoise      string  `json:"denoise"`
}

// Still holds snapshot settings.
type Still struct {
	Quality int `json:"quality"`
}

// Defaults returns the default configuration.
func Defaults() Config {
	return Config{
		Pipeline: Pipeline{StreamSize: StreamSizes[0], BufferCount: 6},
		Encoder:  Encoder{Bitrate: 6_000_000, GOP: 30},
		Controls: Controls{
			FPS: 30, AutoExposure: true, ShutterUS: 10_000, Gain: 1,
			Metering: "centre", ExposureMode: "normal",
			AutoWB: true, AWBMode: "auto", WBGainR: 1.5, WBGainB: 1.5,
			Contrast: 1, Saturation: 1, Sharpness: 1, Denoise: "auto",
		},
		Still: Still{Quality: 95},
		MJPEG: MJPEG{Bitrate: 8_000_000, Skip: 2},
	}
}

// FieldError is a validation error for one field (JSON path).
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// Merge applies a partial JSON document onto base. Unknown fields are
// rejected.
func Merge(base Config, partial []byte) (Config, error) {
	c := base
	dec := json.NewDecoder(bytes.NewReader(partial))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return base, &FieldError{Field: "", Msg: "invalid JSON: " + err.Error()}
	}
	return c, nil
}

// Validate checks c against Limits and, when hello is non-nil, against the
// control ranges reported by the camera.
func Validate(c Config, hello *proto.Hello) error {
	okSize := false
	for _, s := range StreamSizes {
		if c.Pipeline.StreamSize == s {
			okSize = true
		}
	}
	if !okSize {
		return &FieldError{"pipeline.stream_size", fmt.Sprintf("must be one of %v", StreamSizes)}
	}
	L := Limits
	checks := []error{
		rangeI("pipeline.buffer_count", c.Pipeline.BufferCount, L.BufferMin, L.BufferMax),
		rangeI("encoder.bitrate", c.Encoder.Bitrate, L.BitrateMin, L.BitrateMax),
		rangeI("encoder.gop", c.Encoder.GOP, L.GOPMin, L.GOPMax),
		rangeF("controls.fps", c.Controls.FPS, L.FPSMin, L.FPSMax),
		rangeI("controls.shutter_us", c.Controls.ShutterUS, L.ShutterMin, L.ShutterMax),
		rangeF("controls.gain", c.Controls.Gain, L.GainMin, L.GainMax),
		rangeF("controls.ev", c.Controls.EV, L.EVMin, L.EVMax),
		rangeF("controls.wb_gain_r", c.Controls.WBGainR, L.WBGainMin, L.WBGainMax),
		rangeF("controls.wb_gain_b", c.Controls.WBGainB, L.WBGainMin, L.WBGainMax),
		rangeF("controls.brightness", c.Controls.Brightness, L.BrightnessMin, L.BrightnessMax),
		rangeF("controls.contrast", c.Controls.Contrast, 0, L.ContrastMax),
		rangeF("controls.saturation", c.Controls.Saturation, 0, L.SaturationMax),
		rangeF("controls.sharpness", c.Controls.Sharpness, 0, L.SharpnessMax),
		oneOf("controls.metering", c.Controls.Metering, MeteringModes),
		oneOf("controls.exposure_mode", c.Controls.ExposureMode, ExposureModes),
		oneOf("controls.awb_mode", c.Controls.AWBMode, AWBModes),
		oneOf("controls.denoise", c.Controls.Denoise, DenoiseModes),
		rangeI("still.quality", c.Still.Quality, L.QualityMin, L.QualityMax),
		rangeI("mjpeg.bitrate", c.MJPEG.Bitrate, L.MJPEGBitrateMin, L.MJPEGBitrateMax),
		rangeI("mjpeg.skip", c.MJPEG.Skip, 1, L.MJPEGSkipMax),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	if hello != nil {
		// Camera-reported maxima can be stricter than our limits.
		if ci, ok := hello.Camera.Controls["AnalogueGain"]; ok {
			if max, ok := ci.MaxFloat(); ok && max > 0 && c.Controls.Gain > max {
				return &FieldError{"controls.gain", fmt.Sprintf("camera maximum is %g", max)}
			}
		}
		if ci, ok := hello.Camera.Controls["ExposureTime"]; ok {
			if max, ok := ci.MaxFloat(); ok && max > 0 && float64(c.Controls.ShutterUS) > max {
				return &FieldError{"controls.shutter_us", fmt.Sprintf("camera maximum is %g", max)}
			}
		}
	}
	return nil
}

func rangeI(f string, v, lo, hi int) error {
	if v < lo || v > hi {
		return &FieldError{f, fmt.Sprintf("must be in [%d, %d]", lo, hi)}
	}
	return nil
}

func rangeF(f string, v, lo, hi float64) error {
	if math.IsNaN(v) || v < lo || v > hi {
		return &FieldError{f, fmt.Sprintf("must be in [%g, %g]", lo, hi)}
	}
	return nil
}

func oneOf(f, v string, allowed []string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return &FieldError{f, fmt.Sprintf("must be one of %v", allowed)}
}

// ApplyKind says how a config change is applied.
type ApplyKind string

// Apply kinds, from least to most disruptive.
const (
	ApplyNone        ApplyKind = "none"
	ApplyLive        ApplyKind = "live"
	ApplyEncoder     ApplyKind = "encoder"
	ApplyReconfigure ApplyKind = "reconfigure"
)

// Diff returns the most disruptive kind of change between old and new.
// A frame-rate change restarts the encoder too, because its rate control
// needs the frame rate (M0 spike 0.9: bitrate +33 % at 40 fps otherwise).
func Diff(old, new Config) ApplyKind {
	switch {
	case old.Pipeline != new.Pipeline:
		return ApplyReconfigure
	case old.Encoder != new.Encoder || FrameDurationUS(old.Controls) != FrameDurationUS(new.Controls):
		return ApplyEncoder
	case old.Controls != new.Controls:
		return ApplyLive
	}
	return ApplyNone
}
