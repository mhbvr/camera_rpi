package camera

// Schema describes every configuration field for clients that build their
// own UI (GET /api/schema, docs/REST_API.md). Ranges come from Limits, so
// they are exactly what Validate accepts (checked by TestSchemaMatchesValidate).

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

func enum(path string, values []string) Field {
	return Field{Path: path, Type: "enum", Values: values, Apply: "live"}
}

// Schema returns the description of all configuration fields, in UI order.
func Schema() SchemaDoc {
	L := Limits
	sizes := make([][2]int, len(StreamSizes))
	for i, s := range StreamSizes {
		sizes[i] = [2]int{s[0], s[1]}
	}
	f := func(v int) float64 { return float64(v) }
	fields := []Field{
		{Path: "pipeline.stream_size", Type: "size", Sizes: sizes, Unit: "px", Apply: "reconfigure",
			Note: "H.264 and MJPEG stream size; snapshots are always main_size"},
		num("pipeline.buffer_count", "int", f(L.BufferMin), f(L.BufferMax), "", "reconfigure", "camera buffers (CMA memory)"),
		num("encoder.bitrate", "int", f(L.BitrateMin), f(L.BitrateMax), "bit/s", "encoder", "delivered H.264 bitrate"),
		num("encoder.gop", "int", f(L.GOPMin), f(L.GOPMax), "frames", "encoder", "keyframe interval"),
		num("controls.fps", "float", L.FPSMin, L.FPSMax, "fps", "encoder", "also restarts the encoder: its rate control needs the frame rate"),
		{Path: "controls.auto_exposure", Type: "bool", Apply: "live"},
		num("controls.shutter_us", "int", f(L.ShutterMin), f(L.ShutterMax), "µs", "live", "manual exposure; limited by the frame time"),
		num("controls.gain", "float", L.GainMin, L.GainMax, "", "live", "manual exposure; the camera may allow less (status camera.sensor_max_gain)"),
		num("controls.ev", "float", L.EVMin, L.EVMax, "EV", "live", "auto exposure only"),
		enum("controls.metering", MeteringModes),
		enum("controls.exposure_mode", ExposureModes),
		{Path: "controls.auto_wb", Type: "bool", Apply: "live"},
		enum("controls.awb_mode", AWBModes),
		num("controls.wb_gain_r", "float", L.WBGainMin, L.WBGainMax, "", "live", "manual white balance"),
		num("controls.wb_gain_b", "float", L.WBGainMin, L.WBGainMax, "", "live", "manual white balance"),
		num("controls.brightness", "float", L.BrightnessMin, L.BrightnessMax, "", "live", ""),
		num("controls.contrast", "float", 0, L.ContrastMax, "", "live", "1 = normal"),
		num("controls.saturation", "float", 0, L.SaturationMax, "", "live", "1 = normal, 0 = greyscale"),
		num("controls.sharpness", "float", 0, L.SharpnessMax, "", "live", "1 = normal"),
		enum("controls.denoise", DenoiseModes),
		num("still.quality", "int", f(L.QualityMin), f(L.QualityMax), "", "capture", "JPEG quality of snapshots"),
		num("mjpeg.bitrate", "int", f(L.MJPEGBitrateMin), f(L.MJPEGBitrateMax), "bit/s", "mjpeg", "delivered MJPEG fallback bitrate"),
		num("mjpeg.skip", "int", 1, f(L.MJPEGSkipMax), "frames", "mjpeg", "MJPEG encodes every n-th camera frame"),
	}
	return SchemaDoc{
		Fields:     fields,
		Defaults:   Defaults(),
		SensorSize: [2]int{SensorSize[0], SensorSize[1]},
		MainSize:   [2]int{MainSize[0], MainSize[1]},
	}
}
