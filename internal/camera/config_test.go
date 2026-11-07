package camera

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

func TestDefaultsValid(t *testing.T) {
	if err := Validate(Defaults(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestMergeAndValidate(t *testing.T) {
	c, err := Merge(Defaults(), []byte(`{"controls":{"fps":25,"auto_exposure":false},"pipeline":{"stream_size":[1024,768]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Controls.FPS != 25 || c.Controls.AutoExposure || c.Pipeline.StreamSize != (proto.Size{1024, 768}) {
		t.Fatalf("merge result %+v", c)
	}
	if c.Controls.Contrast != 1 || c.Pipeline.BufferCount != 6 {
		t.Fatal("unrelated fields changed")
	}
	if err := Validate(c, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := Merge(Defaults(), []byte(`{"controls":{"nope":1}}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	bad := []string{
		`{"controls":{"fps":100}}`,
		`{"pipeline":{"stream_size":[1920,1080]}}`,
		`{"controls":{"metering":"weird"}}`,
		`{"encoder":{"bitrate":1}}`,
		`{"still":{"quality":0}}`,
	}
	for _, b := range bad {
		c, err := Merge(Defaults(), []byte(b))
		if err != nil {
			t.Fatal(err)
		}
		var fe *FieldError
		if err := Validate(c, nil); !errors.As(err, &fe) {
			t.Errorf("%s: want FieldError, got %v", b, err)
		}
	}
}

func TestValidateHelloLimits(t *testing.T) {
	h := &proto.Hello{Camera: proto.CameraInfo{Controls: map[string]proto.ControlInfo{
		"AnalogueGain": {Min: json.RawMessage("1.0"), Max: json.RawMessage("16.0")},
	}}}
	c := Defaults()
	c.Controls.Gain = 20
	if err := Validate(c, h); err == nil {
		t.Fatal("gain above camera max accepted")
	}
}

func TestDiff(t *testing.T) {
	a := Defaults()
	b := a
	if Diff(a, b) != ApplyNone {
		t.Fatal("none")
	}
	b.Controls.Contrast = 2
	if Diff(a, b) != ApplyLive {
		t.Fatal("live")
	}
	f := a
	f.Controls.FPS = 40
	if Diff(a, f) != ApplyEncoder {
		t.Fatal("fps change must restart the encoder (rate control)")
	}
	m := a
	m.Controls.AutoExposure, m.Controls.ShutterUS = false, 20_000 // fits in 1/30 s: frame rate unchanged
	if Diff(a, m) != ApplyLive {
		t.Fatal("manual shutter within the frame time should be live")
	}
	if r := EncoderFrameRate(f.Controls); r != 40 {
		t.Fatalf("encoder frame rate %v", r)
	}
	b.Encoder.Bitrate = 1_000_000
	if Diff(a, b) != ApplyEncoder {
		t.Fatal("encoder")
	}
	b.Pipeline.StreamSize = StreamSizes[2]
	if Diff(a, b) != ApplyReconfigure {
		t.Fatal("reconfigure")
	}
	c := a
	c.Still.Quality = 80
	if Diff(a, c) != ApplyNone {
		t.Fatal("still change should not touch the camera")
	}
}

// testHello describes the HQ camera as the real helper reports it (spike 0.1).
func testHello() *proto.Hello {
	enum := func(names ...string) proto.ControlInfo {
		v := map[string]int{}
		for i, n := range names {
			v[n] = i
		}
		return proto.ControlInfo{Values: v}
	}
	return &proto.Hello{Camera: proto.CameraInfo{Controls: map[string]proto.ControlInfo{
		"AeEnable": {}, "ExposureTime": {}, "AnalogueGain": {}, "ExposureTimeMode": {}, "AnalogueGainMode": {},
		"ExposureValue": {}, "AwbEnable": {}, "ColourGains": {}, "FrameDurationLimits": {},
		"Brightness": {}, "Contrast": {}, "Saturation": {}, "Sharpness": {},
		"AeMeteringMode":     enum("CentreWeighted", "Spot", "Matrix", "Custom"),
		"AeExposureMode":     enum("Normal", "Short", "Long", "Custom"),
		"AwbMode":            enum("Auto", "Incandescent", "Tungsten", "Fluorescent", "Indoor", "Daylight", "Cloudy", "Custom"),
		"NoiseReductionMode": enum("Off", "Fast", "HighQuality", "Minimal", "ZSL"),
	}}}
}

func TestControlMap(t *testing.T) {
	h := testHello()
	c := Defaults().Controls
	m := ControlMap(c, h)
	if m["AeEnable"] != true || m["ExposureTimeMode"] != 0 || m["AnalogueGainMode"] != 0 || m["AwbMode"] != 0 {
		t.Fatalf("auto map %v", m)
	}
	if _, ok := m["ExposureTime"]; ok {
		t.Fatal("ExposureTime must not be sent in auto mode")
	}
	if _, ok := m["ColourGains"]; ok {
		t.Fatal("ColourGains with auto WB")
	}
	if m["NoiseReductionMode"] != 1 {
		t.Fatalf("denoise auto must send Fast (1), got %v", m["NoiseReductionMode"])
	}
	fd := m["FrameDurationLimits"].([]int)
	if fd[0] != 33333 || fd[1] != 33333 {
		t.Fatalf("frame duration %v", fd)
	}

	c.AutoExposure, c.ShutterUS, c.Gain = false, 100_000, 4
	c.AutoWB, c.WBGainR, c.WBGainB = false, 2, 1.2
	c.Metering, c.Denoise, c.AWBMode = "spot", "high_quality", "daylight"
	m = ControlMap(c, h)
	if m["AeEnable"] != false || m["ExposureTimeMode"] != 1 || m["ExposureTime"] != 100_000 || m["AnalogueGain"] != 4.0 {
		t.Fatalf("manual exposure %v", m)
	}
	if fd := m["FrameDurationLimits"].([]int); fd[0] != 100_000 {
		t.Fatalf("frame duration should stretch to shutter: %v", fd)
	}
	if g := m["ColourGains"].([]float64); g[0] != 2 || g[1] != 1.2 {
		t.Fatalf("gains %v", g)
	}
	if m["AeMeteringMode"] != 1 || m["NoiseReductionMode"] != 2 {
		t.Fatalf("enums %v", m)
	}
}

func TestControlMapUsesCameraValues(t *testing.T) {
	h := &proto.Hello{Camera: proto.CameraInfo{Controls: map[string]proto.ControlInfo{
		"AeEnable": {}, "ExposureTimeMode": {}, "AnalogueGainMode": {},
		"AwbMode": {Values: map[string]int{"Auto": 0, "Daylight": 9}}, "AwbEnable": {},
	}}}
	c := Defaults().Controls
	c.AWBMode = "daylight"
	m := ControlMap(c, h)
	if m["AwbMode"] != 9 {
		t.Fatalf("enum from hello: %v", m["AwbMode"])
	}
	if _, ok := m["Contrast"]; ok {
		t.Fatal("unsupported control not filtered")
	}
	c.AWBMode = "cloudy" // not offered by this camera
	if _, ok := ControlMap(c, h)["AwbMode"]; ok {
		t.Fatal("unknown enum value sent")
	}
}

func TestChangedControls(t *testing.T) {
	a := ControlMap(Defaults().Controls, testHello())
	c := Defaults().Controls
	c.Contrast = 1.5
	ch := ChangedControls(a, ControlMap(c, testHello()))
	if len(ch) != 1 || ch["Contrast"] != 1.5 {
		t.Fatalf("changed %v", ch)
	}
}

func TestStore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.json")
	c, err := LoadConfig(p)
	if err != nil || c != Defaults() {
		t.Fatalf("missing file: %v %+v", err, c)
	}
	c.Controls.EV = 1.5
	if err := SaveConfig(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(p)
	if err != nil || got != c {
		t.Fatalf("round trip: %v %+v", err, got)
	}
}
