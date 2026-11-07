package camera

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
)

// The settings schema is the single definition of every configuration
// field: its type, range or allowed values, and what a change costs. Validate
// checks a Config against it, GET /api/schema publishes it, and the page
// template takes its form ranges from it (docs/REST_API.md, HARDWARE.md §4).

// Field describes one configuration field, addressed by its JSON path.
type Field struct {
	Path   string   `json:"path"`             // e.g. "controls.fps"
	Type   string   `json:"type"`             // int | float | bool | enum | size
	Min    *float64 `json:"min,omitempty"`    // int, float
	Max    *float64 `json:"max,omitempty"`    // int, float
	Values []string `json:"values,omitempty"` // enum
	Sizes  [][2]int `json:"sizes,omitempty"`  // size: allowed [width, height]
	Unit   string   `json:"unit,omitempty"`
	Apply  string   `json:"apply"` // what a change does: reconfigure | encoder | live | capture | mjpeg
	Note   string   `json:"note,omitempty"`
}

// SchemaDoc is the GET /api/schema response.
type SchemaDoc struct {
	Fields     []Field `json:"fields"`
	Defaults   Config  `json:"defaults"`
	SensorSize [2]int  `json:"sensor_size"` // the one sensor mode used for everything
	MainSize   [2]int  `json:"main_size"`   // snapshot size
}

func num(path, typ string, lo, hi float64, unit, apply, note string) Field {
	return Field{Path: path, Type: typ, Min: &lo, Max: &hi, Unit: unit, Apply: apply, Note: note}
}

func enum(path string, values ...string) Field {
	return Field{Path: path, Type: "enum", Values: values, Apply: "live"}
}

func boolean(path string) Field { return Field{Path: path, Type: "bool", Apply: "live"} }

// fields lists every configuration field in UI order.
var fields = []Field{
	{Path: "pipeline.stream_size", Type: "size", Sizes: sizes(StreamSizes), Unit: "px", Apply: "reconfigure",
		Note: "H.264 and MJPEG stream size; snapshots are always main_size"},
	num("pipeline.buffer_count", "int", 2, 12, "", "reconfigure", "camera buffers (CMA memory)"),
	num("encoder.bitrate", "int", 250_000, 25_000_000, "bit/s", "encoder", "delivered H.264 bitrate"),
	num("encoder.gop", "int", 1, 300, "frames", "encoder", "keyframe interval"),
	num("controls.fps", "float", 1, 40, "fps", "encoder", "also restarts the encoder: its rate control needs the frame rate"),
	boolean("controls.auto_exposure"),
	num("controls.shutter_us", "int", 31, 1_000_000, "µs", "live", "manual exposure; limited by the frame time"),
	num("controls.gain", "float", 1, 22, "", "live", "manual exposure; the camera may allow less (status camera.sensor_max_gain)"),
	num("controls.ev", "float", -8, 8, "EV", "live", "auto exposure only"),
	enum("controls.metering", "centre", "spot", "matrix"),
	enum("controls.exposure_mode", "normal", "short", "long"),
	boolean("controls.auto_wb"),
	enum("controls.awb_mode", "auto", "incandescent", "tungsten", "fluorescent", "indoor", "daylight", "cloudy"),
	num("controls.wb_gain_r", "float", 0.1, 32, "", "live", "manual white balance"),
	num("controls.wb_gain_b", "float", 0.1, 32, "", "live", "manual white balance"),
	num("controls.brightness", "float", -1, 1, "", "live", ""),
	num("controls.contrast", "float", 0, 32, "", "live", "1 = normal"),
	num("controls.saturation", "float", 0, 32, "", "live", "1 = normal, 0 = greyscale"),
	num("controls.sharpness", "float", 0, 16, "", "live", "1 = normal"),
	enum("controls.denoise", "auto", "off", "fast", "high_quality", "minimal"),
	num("still.quality", "int", 1, 100, "", "capture", "JPEG quality of snapshots"),
	num("mjpeg.bitrate", "int", 1_000_000, 25_000_000, "bit/s", "mjpeg", "delivered MJPEG fallback bitrate"),
	num("mjpeg.skip", "int", 1, 10, "frames", "mjpeg", "MJPEG encodes every n-th camera frame"),
}

func sizes(s []Size) [][2]int {
	out := make([][2]int, len(s))
	for i, v := range s {
		out[i] = [2]int(v)
	}
	return out
}

// Schema returns the description of all configuration fields, in UI order.
func Schema() SchemaDoc {
	return SchemaDoc{
		Fields:     slices.Clone(fields),
		Defaults:   Defaults(),
		SensorSize: [2]int(SensorSize),
		MainSize:   [2]int(MainSize),
	}
}

// FieldByPath returns the schema entry for path. It panics for an unknown
// path, which is a programming error.
func FieldByPath(path string) Field {
	for _, f := range fields {
		if f.Path == path {
			return f
		}
	}
	panic("camera: no schema field " + path)
}

// Check validates one value (as decoded from JSON: float64, bool, string or
// []any) against the field.
func (f Field) Check(v any) error {
	switch f.Type {
	case "int", "float":
		x, ok := v.(float64)
		if !ok || math.IsNaN(x) || x < *f.Min || x > *f.Max {
			return &FieldError{f.Path, fmt.Sprintf("must be in [%s, %s]", fmtNum(*f.Min), fmtNum(*f.Max))}
		}
	case "enum":
		if s, _ := v.(string); !slices.Contains(f.Values, s) {
			return &FieldError{f.Path, fmt.Sprintf("must be one of %v", f.Values)}
		}
	case "size":
		var got [2]int
		if b, err := json.Marshal(v); err != nil || json.Unmarshal(b, &got) != nil || !slices.Contains(f.Sizes, got) {
			return &FieldError{f.Path, fmt.Sprintf("must be one of %v", f.Sizes)}
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return &FieldError{f.Path, "must be true or false"}
		}
	}
	return nil
}

func fmtNum(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

// flatten returns the config's fields by path ("controls.fps" → value), with
// values as encoding/json decodes them.
func flatten(c Config) map[string]any {
	b, _ := json.Marshal(c)
	var sections map[string]map[string]any
	_ = json.Unmarshal(b, &sections)
	out := map[string]any{}
	for sec, kv := range sections {
		for k, v := range kv {
			out[sec+"."+k] = v
		}
	}
	return out
}
