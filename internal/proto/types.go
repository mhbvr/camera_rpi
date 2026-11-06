package proto

import "encoding/json"

// ProtoVersion is the protocol version this server speaks.
const ProtoVersion = 1

// Method names (requests).
const (
	MethodConfigure   = "configure"
	MethodSetControls = "set_controls"
	MethodSetEncoder  = "set_encoder"
	MethodSnapshot    = "snapshot"
	MethodStop        = "stop"
	MethodShutdown    = "shutdown"
)

// Notification names.
const (
	NoteHello    = "hello"
	NoteState    = "state"
	NoteMetadata = "metadata"
	NoteStats    = "stats"
	NoteError    = "error"
)

// Size is [width, height].
type Size [2]int

// Hello is the first notification sent by the helper.
type Hello struct {
	Proto       int        `json:"proto"`
	Impl        string     `json:"impl"`
	ImplVersion string     `json:"impl_version"`
	Libcamera   string     `json:"libcamera"`
	Camera      CameraInfo `json:"camera"`
	Features    []string   `json:"features"`
	Tuning      *Tuning    `json:"tuning,omitempty"` // nil if the tuning file could not be read
}

// Tuning is the exposure-related part of the libcamera tuning file in use
// (rpi.agc). libcamera's camera API does not expose it; the helper reads the
// file the IPA loads for the sensor.
type Tuning struct {
	File           string                     `json:"file"`
	ExposureModes  map[string]ExposureProfile `json:"exposure_modes"`
	MeteringModes  map[string][]float64       `json:"metering_modes"` // name -> per-zone weights
	YTarget        []float64                  `json:"y_target"`       // pairs: lux, target brightness (0..1)
	MaxDigitalGain float64                    `json:"max_digital_gain"`
}

// ExposureProfile lists the shutter (µs) and gain steps auto exposure walks
// through, in order.
type ExposureProfile struct {
	Shutter []float64 `json:"shutter"`
	Gain    []float64 `json:"gain"`
}

// CameraInfo describes the camera.
type CameraInfo struct {
	Model         string                 `json:"model"`
	PixelArray    Size                   `json:"pixel_array"`
	ScalerCropMax [4]int                 `json:"scaler_crop_max"`
	SensorModes   []SensorMode           `json:"sensor_modes"`
	Controls      map[string]ControlInfo `json:"controls"`
}

// SensorMode is one sensor mode reported by libcamera.
type SensorMode struct {
	Size           Size            `json:"size"`
	BitDepth       int             `json:"bit_depth"`
	FPS            float64         `json:"fps"`
	CropLimits     [4]int          `json:"crop_limits"`
	ExposureLimits json.RawMessage `json:"exposure_limits,omitempty"`
}

// ControlInfo is the range of one libcamera control. Min/Max/Default can be
// numbers, arrays or null, so they are kept raw.
type ControlInfo struct {
	Min     json.RawMessage `json:"min"`
	Max     json.RawMessage `json:"max"`
	Default json.RawMessage `json:"default"`
	Values  map[string]int  `json:"values,omitempty"`
}

// MinFloat returns Min as a number, if it is one.
func (c ControlInfo) MinFloat() (float64, bool) { return rawFloat(c.Min) }

// MaxFloat returns Max as a number, if it is one.
func (c ControlInfo) MaxFloat() (float64, bool) { return rawFloat(c.Max) }

func rawFloat(r json.RawMessage) (float64, bool) {
	var f float64
	if len(r) == 0 || json.Unmarshal(r, &f) != nil {
		return 0, false
	}
	return f, true
}

// ConfigureParams are the params of "configure".
type ConfigureParams struct {
	Sensor      SensorCfg      `json:"sensor"`
	Main        StreamCfg      `json:"main"`
	Lores       StreamCfg      `json:"lores"`
	Transform   Transform      `json:"transform"`
	BufferCount int            `json:"buffer_count"`
	Controls    map[string]any `json:"controls"`
	Encoder     EncoderCfg     `json:"encoder"`
	MetadataHz  float64        `json:"metadata_hz"`
}

// SensorCfg selects the sensor mode.
type SensorCfg struct {
	Size     Size `json:"size"`
	BitDepth int  `json:"bit_depth"`
}

// StreamCfg configures one ISP output.
type StreamCfg struct {
	Size   Size   `json:"size"`
	Format string `json:"format"`
	Stride int    `json:"stride,omitempty"`
}

// Transform holds the flips.
type Transform struct {
	HFlip bool `json:"hflip"`
	VFlip bool `json:"vflip"`
}

// EncoderCfg configures the H.264 encoder.
type EncoderCfg struct {
	Codec     string  `json:"codec"`
	Bitrate   int     `json:"bitrate"`
	IPeriod   int     `json:"iperiod"`
	FrameRate float64 `json:"framerate"` // for the encoder's rate control
}

// ConfigureResult is the result of "configure".
type ConfigureResult struct {
	Applied json.RawMessage `json:"applied"`
	Session int             `json:"session"`
}

// SetControlsParams are the params of "set_controls".
type SetControlsParams struct {
	Controls map[string]any `json:"controls"`
}

// SetEncoderParams are the params of "set_encoder".
type SetEncoderParams struct {
	Bitrate   int     `json:"bitrate"`
	IPeriod   int     `json:"iperiod"`
	FrameRate float64 `json:"framerate"`
}

// SetEncoderResult is the result of "set_encoder".
type SetEncoderResult struct {
	Session int `json:"session"`
}

// SnapshotParams are the params of "snapshot".
type SnapshotParams struct {
	Path    string `json:"path"`
	Quality int    `json:"quality"`
}

// SnapshotResult is the result of "snapshot".
type SnapshotResult struct {
	File     FileInfo           `json:"file"`
	Metadata map[string]any     `json:"metadata,omitempty"`
	TimingMS map[string]float64 `json:"timing_ms,omitempty"`
}

// FileInfo describes a written file.
type FileInfo struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// StateParams are the params of the "state" notification.
type StateParams struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// StatsParams are the params of the "stats" notification.
type StatsParams struct {
	FPS       float64  `json:"fps"`
	Frames    int64    `json:"frames"`
	LatencyMS *float64 `json:"latency_ms,omitempty"` // sensor timestamp -> socket write
}

// StreamStart is the JSON payload of a VideoStreamStart message.
type StreamStart struct {
	Session int     `json:"session"`
	Codec   string  `json:"codec"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	FPS     float64 `json:"fps"`
	Profile string  `json:"profile"`
	Bitrate int     `json:"bitrate"`
	IPeriod int     `json:"iperiod"`
}

// StreamEnd is the JSON payload of a VideoStreamEnd message.
type StreamEnd struct {
	Session int    `json:"session"`
	Reason  string `json:"reason"`
}
