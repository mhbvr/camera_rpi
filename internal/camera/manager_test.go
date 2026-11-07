package camera

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhbvr/camera_rpi/internal/helper"
	"github.com/mhbvr/camera_rpi/internal/proto"
	"github.com/mhbvr/camera_rpi/internal/stream"
)

type call struct {
	Method string
	Params json.RawMessage
}

// fakeCam is a minimal helper that records calls.
type fakeCam struct {
	mu    sync.Mutex
	calls []call
	ctl   net.Conn
}

func (f *fakeCam) record(m *proto.Incoming) {
	f.mu.Lock()
	f.calls = append(f.calls, call{m.Method, m.Params})
	f.mu.Unlock()
}

func (f *fakeCam) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeCam) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func startFake(t *testing.T, l *helper.Listener) *fakeCam {
	ctl, err := net.Dial("unix", l.CtlPath())
	if err != nil {
		t.Fatal(err)
	}
	video, err := net.Dial("unix", l.VideoPath())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCam{ctl: ctl}
	t.Cleanup(func() { ctl.Close(); video.Close() })
	write := func(v any) {
		b, _ := proto.MarshalLine(v)
		ctl.Write(b)
	}
	hello := testHello()
	hello.Proto, hello.Impl, hello.Camera.Model = proto.ProtoVersion, "fake", "imx477"
	helloJSON, _ := json.Marshal(hello)
	write(proto.Notification{JSONRPC: "2.0", Method: "hello", Params: helloJSON})
	go func() {
		sc := proto.NewLineScanner(ctl)
		for sc.Scan() {
			m, _ := proto.ParseIncoming(sc.Bytes())
			f.record(m)
			res := json.RawMessage(`{}`)
			switch m.Method {
			case proto.MethodConfigure:
				res = json.RawMessage(`{"applied":{},"session":1}`)
			case proto.MethodSnapshot:
				var p proto.SnapshotParams
				_ = json.Unmarshal(m.Params, &p)
				_ = os.WriteFile(p.Path+".jpg", []byte("jpeg"), 0o644)
				b, _ := json.Marshal(proto.SnapshotResult{File: proto.FileInfo{Path: p.Path + ".jpg", Size: 4, Width: 2028, Height: 1520}})
				res = b
			}
			write(proto.Response{JSONRPC: "2.0", ID: m.ID, Result: res})
		}
	}()
	return f
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerFlow(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	l, err := helper.Listen(filepath.Join(dir, "run"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	hub := stream.NewHub(8)
	m := NewManager(Defaults(), hub, Options{
		ConfigPath:  filepath.Join(dir, "config.json"),
		CapturesDir: filepath.Join(dir, "captures"),
		HFlip:       true,
		Log:         logger,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		s, err := l.Accept(ctx, m.OnVideo)
		if err != nil {
			return
		}
		_ = m.RunSession(ctx, s)
	}()
	f := startFake(t, l)

	// configure with the full config
	waitFor(t, func() bool { return f.count() >= 1 })
	c := f.last()
	var cp proto.ConfigureParams
	_ = json.Unmarshal(c.Params, &cp)
	if c.Method != proto.MethodConfigure || cp.Lores.Size != (proto.Size{1440, 1080}) || !cp.Transform.HFlip || cp.Encoder.Bitrate != 6_000_000 {
		t.Fatalf("configure call %s %+v", c.Method, cp)
	}
	waitFor(t, func() bool { m.mu.RLock(); defer m.mu.RUnlock(); return m.sess != nil })

	// new viewers get the sticky "camera" event (exposure profile info for the UI)
	sub := hub.Subscribe()
	gotCamera := false
	msgs, _ := sub.Take()
	for _, msg := range msgs {
		if strings.Contains(string(msg.Payload()), `"event":"camera"`) {
			gotCamera = true
		}
	}
	hub.Unsubscribe(sub)
	if !gotCamera {
		t.Fatal("no camera event for new viewers")
	}

	// live change → set_controls with only the changed control
	kind, _, err := m.Apply(ctx, []byte(`{"controls":{"contrast":2}}`))
	if err != nil || kind != ApplyLive {
		t.Fatalf("apply live: %v %v", kind, err)
	}
	c = f.last()
	var sc proto.SetControlsParams
	_ = json.Unmarshal(c.Params, &sc)
	if c.Method != proto.MethodSetControls || len(sc.Controls) != 1 || sc.Controls["Contrast"] != 2.0 {
		t.Fatalf("set_controls call %s %v", c.Method, sc.Controls)
	}

	// encoder change
	kind, _, err = m.Apply(ctx, []byte(`{"encoder":{"bitrate":3000000}}`))
	if err != nil || kind != ApplyEncoder || f.last().Method != proto.MethodSetEncoder {
		t.Fatalf("apply encoder: %v %v %s", kind, err, f.last().Method)
	}

	// fps change → set_controls (FrameDurationLimits) then set_encoder with the frame rate
	n0 := f.count()
	kind, _, err = m.Apply(ctx, []byte(`{"controls":{"fps":40}}`))
	if err != nil || kind != ApplyEncoder || f.count() != n0+2 {
		t.Fatalf("apply fps: %v %v calls=%d", kind, err, f.count()-n0)
	}
	f.mu.Lock()
	first, second := f.calls[n0], f.calls[n0+1]
	f.mu.Unlock()
	var se proto.SetEncoderParams
	_ = json.Unmarshal(second.Params, &se)
	if first.Method != proto.MethodSetControls || second.Method != proto.MethodSetEncoder || se.FrameRate != 40 {
		t.Fatalf("fps calls: %s, %s %+v", first.Method, second.Method, se)
	}

	// stream size → configure
	kind, _, err = m.Apply(ctx, []byte(`{"pipeline":{"stream_size":[640,480]}}`))
	if err != nil || kind != ApplyReconfigure || f.last().Method != proto.MethodConfigure {
		t.Fatalf("apply reconfigure: %v %v %s", kind, err, f.last().Method)
	}

	// invalid change is rejected and not sent
	n := f.count()
	if _, _, err := m.Apply(ctx, []byte(`{"controls":{"fps":500}}`)); err == nil {
		t.Fatal("invalid fps accepted")
	}
	if f.count() != n {
		t.Fatal("invalid change reached the helper")
	}

	// persisted
	saved, err := LoadConfig(filepath.Join(dir, "config.json"))
	if err != nil || saved.Pipeline.StreamSize != (proto.Size{640, 480}) || saved.Controls.Contrast != 2 {
		t.Fatalf("persisted %+v %v", saved, err)
	}

	// snapshot
	res, err := m.Snapshot(ctx, 0)
	if err != nil || res.Width != 2028 || string(res.JPEG) != "jpeg" || !strings.HasPrefix(res.Name, "IMG_") {
		t.Fatalf("snapshot %+v %v", res, err)
	}
	var sp proto.SnapshotParams
	_ = json.Unmarshal(f.last().Params, &sp)
	if sp.Quality != 95 || filepath.Dir(sp.Path) != filepath.Join(dir, "captures") {
		t.Fatalf("snapshot params %+v", sp)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "captures")); len(left) != 0 {
		t.Fatalf("capture file not deleted: %v", left)
	}
	// a second snapshot in the same second gets a unique name
	if _, err := m.Snapshot(ctx, 80); err != nil {
		t.Fatal(err)
	}
	var sp2 proto.SnapshotParams
	_ = json.Unmarshal(f.last().Params, &sp2)
	if sp2.Path == sp.Path {
		t.Fatal("snapshot name not unique")
	}
}

func TestManagerNoCamera(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Defaults(), stream.NewHub(4), Options{ConfigPath: filepath.Join(dir, "c.json"), Log: log.New(io.Discard, "", 0)})
	if _, err := m.Snapshot(context.Background(), 0); err != ErrNoCamera {
		t.Fatalf("want ErrNoCamera, got %v", err)
	}
	kind, cfg, err := m.Apply(context.Background(), []byte(`{"controls":{"ev":1}}`))
	if err != nil || kind != ApplyLive || cfg.Controls.EV != 1 {
		t.Fatalf("store-only apply: %v %v", kind, err)
	}
}

func TestManagerMJPEG(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	l, err := helper.Listen(filepath.Join(dir, "run"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m := NewManager(Defaults(), stream.NewHub(8), Options{ConfigPath: filepath.Join(dir, "c.json"), Log: logger})
	frames := m.Frames()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if s, err := l.Accept(ctx, m.OnVideo); err == nil {
			_ = m.RunSession(ctx, s)
		}
	}()
	f := startFake(t, l)
	waitFor(t, func() bool { m.mu.RLock(); defer m.mu.RUnlock(); return m.sess != nil })

	lastMJPEG := func() (proto.SetMJPEGParams, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := len(f.calls) - 1; i >= 0; i-- {
			if f.calls[i].Method == proto.MethodSetMJPEG {
				var p proto.SetMJPEGParams
				_ = json.Unmarshal(f.calls[i].Params, &p)
				return p, true
			}
		}
		return proto.SetMJPEGParams{}, false
	}

	sub := frames.Subscribe() // first viewer → encoder on
	waitFor(t, func() bool {
		p, ok := lastMJPEG()
		return ok && p.Enabled && p.Bitrate == 8_000_000 && p.Skip == 2 && p.FrameRate == 30
	})

	// JPEG frames from the helper reach the viewer
	m.OnVideo(&proto.VideoMsg{Type: proto.VideoJPEG, Payload: []byte{0xff, 0xd8}})
	if b := <-sub.C; len(b) != 2 {
		t.Fatalf("frame %x", b)
	}

	// changing MJPEG settings while active restarts the encoder with them
	if _, _, err := m.Apply(ctx, []byte(`{"mjpeg":{"skip":3}}`)); err != nil {
		t.Fatal(err)
	}
	if p, _ := lastMJPEG(); !p.Enabled || p.Skip != 3 {
		t.Fatalf("after settings change: %+v", p)
	}
	if _, _, err := m.Apply(ctx, []byte(`{"mjpeg":{"skip":0}}`)); err == nil {
		t.Fatal("skip 0 accepted")
	}
	// a frame-rate change re-sends set_mjpeg (the helper sets the MJPEG encoder's frame rate from fps)
	n := f.count()
	if _, _, err := m.Apply(ctx, []byte(`{"controls":{"fps":20}}`)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	resent := false
	for _, c := range f.calls[n:] {
		resent = resent || c.Method == proto.MethodSetMJPEG
	}
	f.mu.Unlock()
	if !resent {
		t.Fatal("fps change did not re-send set_mjpeg")
	}
	if p, _ := lastMJPEG(); p.FrameRate != 20 {
		t.Fatalf("set_mjpeg after fps change: %+v", p)
	}

	frames.Unsubscribe(sub) // last viewer gone → encoder off
	waitFor(t, func() bool { p, _ := lastMJPEG(); return !p.Enabled })
}

func TestLeftoverCapturesRemoved(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"IMG_20261107_010101.jpg", "IMG_20261107_010102.jpg.tmp", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	NewManager(Defaults(), stream.NewHub(8), Options{ConfigPath: filepath.Join(dir, "c.json"), CapturesDir: dir, Log: log.New(io.Discard, "", 0)})
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "notes.txt" {
		t.Fatalf("left in captures dir: %v", left)
	}
}
