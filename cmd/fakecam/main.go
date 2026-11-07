// Command fakecam is a fake camera helper for development without a Pi. It
// speaks docs/HELPER_API.md and replays a recorded H.264 stream in a loop.
//
//	camweb -helper-cmd "go run ./cmd/fakecam"
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mhbvr/camera_rpi/internal/h264"
	"github.com/mhbvr/camera_rpi/internal/proto"
)

//go:embed testdata/test.h264
var defaultStream []byte

var dark bool

type fake struct {
	log   *log.Logger
	ctlMu sync.Mutex
	ctl   net.Conn
	video net.Conn
	aus   [][]byte

	mu       sync.Mutex
	session  int
	running  bool
	stopCh   chan struct{}
	params   proto.ConfigureParams
	controls map[string]any
	frames   int64

	mjpegStop chan struct{} // non-nil while the fake MJPEG encoder runs
}

func main() {
	ctlPath := flag.String("ctl", "", "ctl socket path")
	videoPath := flag.String("video", "", "video socket path")
	file := flag.String("file", "", "Annex-B H.264 file to replay (default: embedded 1440x1080 test pattern)")
	flag.BoolVar(&dark, "dark", false, "simulate a dark scene: auto exposure stuck at its limits")
	flag.Parse()
	logger := log.New(os.Stderr, "fakecam: ", 0)

	data := defaultStream
	if *file != "" {
		var err error
		if data, err = os.ReadFile(*file); err != nil {
			logger.Fatal(err)
		}
	}
	f := &fake{log: logger, aus: h264.SplitAccessUnits(data), controls: map[string]any{}}
	if len(f.aus) == 0 {
		logger.Fatal("no access units in stream")
	}
	var err error
	if f.ctl, err = dialRetry(*ctlPath); err != nil {
		logger.Fatal(err)
	}
	if f.video, err = dialRetry(*videoPath); err != nil {
		logger.Fatal(err)
	}
	f.notify(proto.NoteHello, helloParams())
	f.notify(proto.NoteState, proto.StateParams{State: "idle"})
	go f.statsLoop()

	lr := proto.NewLineReader(f.ctl)
	for {
		line, err := lr.ReadLine()
		if err != nil {
			logger.Printf("ctl closed: %v", err)
			return
		}
		m, err := proto.ParseIncoming(line)
		if err != nil || !m.IsRequest() {
			logger.Printf("bad request: %s", line)
			continue
		}
		res, rpcErr := f.handle(m)
		resp := proto.Response{JSONRPC: proto.Version, ID: m.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			b, _ := json.Marshal(res)
			resp.Result = b
		}
		f.send(resp)
		if m.Method == proto.MethodShutdown {
			return
		}
	}
}

func dialRetry(path string) (net.Conn, error) {
	var err error
	for i := 0; i < 100; i++ {
		var c net.Conn
		if c, err = net.Dial("unix", path); err == nil {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}

func (f *fake) send(v any) {
	b, _ := proto.MarshalLine(v)
	f.ctlMu.Lock()
	defer f.ctlMu.Unlock()
	if _, err := f.ctl.Write(b); err != nil {
		f.log.Fatalf("ctl write: %v", err)
	}
}

func (f *fake) notify(method string, params any) {
	b, _ := json.Marshal(params)
	f.send(proto.Notification{JSONRPC: proto.Version, Method: method, Params: b})
}

func invalid(msg string) *proto.RPCError {
	return &proto.RPCError{Code: proto.CodeInvalidParams, Message: msg}
}

func (f *fake) handle(m *proto.Incoming) (any, *proto.RPCError) {
	switch m.Method {
	case proto.MethodConfigure:
		var p proto.ConfigureParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, invalid(err.Error())
		}
		f.stopStream("reconfigure")
		f.mu.Lock()
		f.params = p
		for k, v := range p.Controls {
			f.controls[k] = v
		}
		f.mu.Unlock()
		f.notify(proto.NoteState, proto.StateParams{State: "starting"})
		session := f.startStream()
		f.notify(proto.NoteState, proto.StateParams{State: "streaming"})
		applied, _ := json.Marshal(map[string]any{"main": p.Main, "lores": p.Lores, "sensor": p.Sensor})
		return proto.ConfigureResult{Applied: applied, Session: session}, nil
	case proto.MethodSetControls:
		var p proto.SetControlsParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, invalid(err.Error())
		}
		f.mu.Lock()
		for k, v := range p.Controls {
			f.controls[k] = v
		}
		f.mu.Unlock()
		f.log.Printf("set_controls %v", p.Controls)
		return struct{}{}, nil
	case proto.MethodSetEncoder:
		var p proto.SetEncoderParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, invalid(err.Error())
		}
		if !f.isRunning() {
			return nil, &proto.RPCError{Code: proto.CodeNotConfigured, Message: "not streaming"}
		}
		f.stopStream("encoder")
		f.mu.Lock()
		f.params.Encoder.Bitrate, f.params.Encoder.IPeriod = p.Bitrate, p.IPeriod
		f.mu.Unlock()
		return proto.SetEncoderResult{Session: f.startStream()}, nil
	case proto.MethodSnapshot:
		var p proto.SnapshotParams
		if err := json.Unmarshal(m.Params, &p); err != nil || p.Path == "" {
			return nil, invalid("path required")
		}
		if !f.isRunning() {
			return nil, &proto.RPCError{Code: proto.CodeNotConfigured, Message: "not streaming"}
		}
		return f.snapshot(p)
	case proto.MethodSetMJPEG:
		var p proto.SetMJPEGParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, invalid(err.Error())
		}
		f.mu.Lock()
		if f.mjpegStop != nil {
			close(f.mjpegStop)
			f.mjpegStop = nil
		}
		if p.Enabled {
			f.mjpegStop = make(chan struct{})
			go f.mjpegLoop(f.mjpegStop, max(1, p.Skip))
		}
		f.mu.Unlock()
		f.log.Printf("set_mjpeg %+v", p)
		return map[string]bool{"active": p.Enabled}, nil
	case proto.MethodStop:
		f.stopStream("stop")
		f.notify(proto.NoteState, proto.StateParams{State: "idle"})
		return struct{}{}, nil
	case proto.MethodShutdown:
		f.stopStream("stop")
		return struct{}{}, nil
	}
	return nil, &proto.RPCError{Code: proto.CodeMethodNotFound, Message: "unknown method " + m.Method}
}

func (f *fake) isRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fake) fps() float64 {
	if d, ok := f.controls["FrameDurationLimits"].([]any); ok && len(d) == 2 {
		if us, ok := d[0].(float64); ok && us > 0 {
			return 1e6 / us
		}
	}
	return 30
}

func (f *fake) startStream() int {
	f.mu.Lock()
	f.session++
	session := f.session
	f.running = true
	stop := make(chan struct{})
	f.stopCh = stop
	p := f.params
	fps := f.fps()
	f.mu.Unlock()

	ss, _ := json.Marshal(proto.StreamStart{Session: session, Codec: "h264", Width: p.Lores.Size[0], Height: p.Lores.Size[1],
		FPS: fps, Profile: "high", Bitrate: p.Encoder.Bitrate, IPeriod: p.Encoder.IPeriod})
	f.writeVideo(&proto.VideoMsg{Type: proto.VideoStreamStart, Payload: ss})
	go f.streamLoop(stop)
	return session
}

func (f *fake) stopStream(reason string) {
	f.mu.Lock()
	if !f.running {
		f.mu.Unlock()
		return
	}
	f.running = false
	close(f.stopCh)
	session := f.session
	f.mu.Unlock()
	se, _ := json.Marshal(proto.StreamEnd{Session: session, Reason: reason})
	f.writeVideo(&proto.VideoMsg{Type: proto.VideoStreamEnd, Payload: se})
}

var videoMu sync.Mutex

func (f *fake) writeVideo(m *proto.VideoMsg) {
	videoMu.Lock()
	defer videoMu.Unlock()
	if err := proto.WriteVideoMsg(f.video, m); err != nil {
		f.log.Fatalf("video write: %v", err)
	}
}

func (f *fake) streamLoop(stop chan struct{}) {
	f.mu.Lock()
	fps := f.fps()
	f.mu.Unlock()
	tick := time.NewTicker(time.Duration(float64(time.Second) / fps))
	defer tick.Stop()
	var seq uint64
	md := time.NewTicker(500 * time.Millisecond)
	defer md.Stop()
	start := time.Now()
	for i := 0; ; i = (i + 1) % len(f.aus) {
		select {
		case <-stop:
			return
		case <-md.C:
			f.notify(proto.NoteMetadata, f.metadata(time.Since(start)))
			<-tick.C
		case <-tick.C:
		}
		au := f.aus[i]
		key, sps := h264.Info(au)
		flags := uint8(0)
		if key {
			flags |= proto.FlagKey
		}
		if sps {
			flags |= proto.FlagConfig
		}
		now := time.Now()
		f.writeVideo(&proto.VideoMsg{Type: proto.VideoAU, Flags: flags, Seq: seq, PTSUS: uint64(now.Sub(start).Microseconds()),
			WallUS: uint64(now.UnixMicro()), Payload: au})
		seq++
		f.mu.Lock()
		f.frames++
		f.mu.Unlock()
	}
}

// mjpegLoop sends JPEG frames of the lores size with a moving bar, at the
// camera frame rate divided by skip.
func (f *fake) mjpegLoop(stop chan struct{}, skip int) {
	f.mu.Lock()
	size := f.params.Lores.Size
	fps := f.fps()
	f.mu.Unlock()
	if size[0] == 0 {
		size = proto.Size{1440, 1080}
	}
	tick := time.NewTicker(time.Duration(float64(time.Second) * float64(skip) / fps))
	defer tick.Stop()
	img := image.NewYCbCr(image.Rect(0, 0, size[0], size[1]), image.YCbCrSubsampleRatio420)
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = 110, 150
	}
	var seq uint64
	for n := 0; ; n++ {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		bar := (n % 30) * size[0] / 30
		for y := 0; y < size[1]; y++ {
			row := img.Y[y*img.YStride : y*img.YStride+size[0]]
			for x := range row {
				if x >= bar && x < bar+size[0]/30 {
					row[x] = 235
				} else {
					row[x] = uint8(40 + (x+y)%150)
				}
			}
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 60}); err != nil {
			continue
		}
		f.writeVideo(&proto.VideoMsg{Type: proto.VideoJPEG, Seq: seq, WallUS: uint64(time.Now().UnixMicro()), Payload: buf.Bytes()})
		seq++
	}
}

func (f *fake) metadata(t time.Duration) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	exp, gain := 8000.0, 2.0
	fd := 1e6 / f.fps()
	auto := f.controls["AeEnable"] != false
	if auto && dark {
		exp, gain = math.Min(fd-300, 66666), 16.0
	}
	aeState := 2
	if !auto {
		aeState = 0
	}
	if !auto {
		if v, ok := f.controls["ExposureTime"].(float64); ok {
			exp = v
		}
		if v, ok := f.controls["AnalogueGain"].(float64); ok {
			gain = v
		}
	}
	gains := []float64{1.8, 1.5}
	if g, ok := f.controls["ColourGains"].([]any); ok && len(g) == 2 {
		gains = []float64{g[0].(float64), g[1].(float64)}
	}
	s := t.Seconds()
	return map[string]any{
		"ExposureTime": exp, "AnalogueGain": gain, "DigitalGain": 1.0, "ColourGains": gains,
		"ColourTemperature": 4800 + 100*math.Sin(s/5), "Lux": 300 + 20*math.Sin(s/3),
		"FrameDuration": int(fd), "FocusFoM": int(1500 + 1000*math.Sin(s/2)), "AeState": aeState,
	}
}

func (f *fake) statsLoop() {
	var last int64
	for range time.Tick(time.Second) {
		f.mu.Lock()
		n, running := f.frames, f.running
		f.mu.Unlock()
		if running {
			f.notify(proto.NoteStats, proto.StatsParams{FPS: float64(n - last), Frames: n})
		}
		last = n
	}
}

func (f *fake) snapshot(p proto.SnapshotParams) (any, *proto.RPCError) {
	t0 := time.Now()
	f.mu.Lock()
	size := f.params.Main.Size
	f.mu.Unlock()
	if size[0] == 0 {
		size = proto.Size{2028, 1520}
	}
	img := image.NewYCbCr(image.Rect(0, 0, size[0], size[1]), image.YCbCrSubsampleRatio420)
	ph := float64(time.Now().UnixMilli()%10000) / 10000
	for y := 0; y < size[1]; y++ {
		for x := 0; x < size[0]; x++ {
			img.Y[y*img.YStride+x] = uint8((x + y + int(ph*255)) % 256)
		}
	}
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = 100, 160
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: p.Quality}); err != nil {
		return nil, &proto.RPCError{Code: proto.CodeInternal, Message: err.Error()}
	}
	path := p.Path + ".jpg"
	if err := os.WriteFile(path+".tmp", buf.Bytes(), 0o644); err != nil {
		return nil, &proto.RPCError{Code: proto.CodeIOError, Message: err.Error()}
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return nil, &proto.RPCError{Code: proto.CodeIOError, Message: err.Error()}
	}
	return proto.SnapshotResult{
		File:     proto.FileInfo{Path: path, Size: int64(buf.Len()), Width: size[0], Height: size[1]},
		Metadata: f.metadata(0),
		TimingMS: map[string]float64{"encode": float64(time.Since(t0).Microseconds()) / 1000},
	}, nil
}

// helloParams describes the camera like the real helper does for the HQ
// camera (controls, ranges and enum names from spike 0.1).
func helloParams() proto.Hello {
	ctl := func(min, max, def string) proto.ControlInfo {
		return proto.ControlInfo{Min: json.RawMessage(min), Max: json.RawMessage(max), Default: json.RawMessage(def)}
	}
	enum := func(names ...string) proto.ControlInfo {
		c := ctl("0", fmt.Sprint(len(names)-1), "0")
		c.Values = map[string]int{}
		for i, n := range names {
			c.Values[n] = i
		}
		return c
	}
	return proto.Hello{
		Proto: proto.ProtoVersion, Impl: "fakecam", ImplVersion: "1", Libcamera: "none",
		Camera: proto.CameraInfo{
			Model: "imx477-fake", PixelArray: proto.Size{4056, 3040}, ScalerCropMax: [4]int{0, 0, 4056, 3040},
			SensorModes: []proto.SensorMode{
				{Size: proto.Size{1332, 990}, BitDepth: 10, FPS: 120.05},
				{Size: proto.Size{2028, 1080}, BitDepth: 12, FPS: 50.03},
				{Size: proto.Size{2028, 1520}, BitDepth: 12, FPS: 40.01},
				{Size: proto.Size{4056, 3040}, BitDepth: 12, FPS: 10},
			},
			Controls: map[string]proto.ControlInfo{
				"AeEnable": ctl("false", "true", "true"), "ExposureTime": ctl("74", "694422939", "20000"),
				"AnalogueGain": ctl("1.0", "22.26", "1.0"), "ExposureValue": ctl("-8.0", "8.0", "0.0"),
				"ExposureTimeMode": enum("Auto", "Manual"), "AnalogueGainMode": enum("Auto", "Manual"),
				"AeMeteringMode": enum("CentreWeighted", "Spot", "Matrix", "Custom"),
				"AeExposureMode": enum("Normal", "Short", "Long", "Custom"),
				"AwbEnable":      ctl("false", "true", "null"),
				"AwbMode":        enum("Auto", "Incandescent", "Tungsten", "Fluorescent", "Indoor", "Daylight", "Cloudy", "Custom"),
				"ColourGains":    ctl("0.0", "32.0", "null"), "Brightness": ctl("-1.0", "1.0", "0.0"),
				"Contrast": ctl("0.0", "32.0", "1.0"), "Saturation": ctl("0.0", "32.0", "1.0"),
				"Sharpness":           ctl("0.0", "16.0", "1.0"),
				"NoiseReductionMode":  enum("Off", "Fast", "HighQuality", "Minimal", "ZSL"),
				"FrameDurationLimits": ctl("57502", "694434742", "null"),
			},
		},
		Features: []string{"snapshot", "metadata", "set_encoder", "mjpeg"},
	}
}
