// Package camera holds the user-facing camera configuration, its mapping to
// libcamera controls, and the Manager that drives the helper.
package camera

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

// Fixed pipeline geometry (HARDWARE.md §2): one sensor mode, 2×2 binned,
// full field of view (confirmed by spike 0.6).
var (
	SensorSize     = proto.Size{2028, 1520}
	SensorBitDepth = 12
	MainSize       = proto.Size{2028, 1520}
	StreamSizes    = []proto.Size{{1440, 1080}, {1024, 768}, {640, 480}}
)

// Size is a [width, height] pair.
type Size = proto.Size

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

// Validate checks every field of c against the schema and, when hello is
// non-nil, against the gain and shutter maxima reported by the camera.
func Validate(c Config, hello *proto.Hello) error {
	values := flatten(c)
	for _, f := range fields {
		if err := f.Check(values[f.Path]); err != nil {
			return err
		}
	}
	if hello == nil {
		return nil
	}
	// The camera's own maxima can be stricter than the schema.
	if max, ok := hello.Camera.Controls["AnalogueGain"].MaxFloat(); ok && max > 0 && c.Controls.Gain > max {
		return &FieldError{"controls.gain", fmt.Sprintf("camera maximum is %g", max)}
	}
	if max, ok := hello.Camera.Controls["ExposureTime"].MaxFloat(); ok && max > 0 && float64(c.Controls.ShutterUS) > max {
		return &FieldError{"controls.shutter_us", fmt.Sprintf("camera maximum is %g", max)}
	}
	return nil
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
