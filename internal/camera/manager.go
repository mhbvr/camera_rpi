package camera

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mhbvr/camera_rpi/internal/helper"
	"github.com/mhbvr/camera_rpi/internal/proto"
	"github.com/mhbvr/camera_rpi/internal/stream"
)

// ErrNoCamera is returned when no helper is connected.
var ErrNoCamera = errors.New("camera helper not connected")

// Options configures a Manager.
type Options struct {
	ConfigPath  string // where the config is persisted
	CapturesDir string // where the helper writes snapshots; files live only until read (use a RAM-backed dir)
	HFlip       bool
	VFlip       bool
	Log         *log.Logger
}

// Manager owns the configuration and drives the helper session. All helper
// operations are serialised.
//
// Locks: opMu is held for a whole helper operation (it may wait for the
// helper); mu guards the fields below it and is only held briefly.
type Manager struct {
	opt    Options
	hub    *stream.Hub      // H.264 and events
	frames *stream.FrameHub // MJPEG fallback viewers

	opMu sync.Mutex // serialises helper operations

	mu    sync.RWMutex
	cfg   Config
	sess  *helper.Session
	hello *proto.Hello

	mjpegWanted bool          // at least one MJPEG viewer
	mjpegSync   chan struct{} // signalled when mjpegWanted changes (capacity 1)

	lastStem string // capture names: IMG_<time>, with _1, _2 … within one second (opMu)
	nameSeq  int
}

// NewManager returns a manager starting from cfg.
func NewManager(cfg Config, hub *stream.Hub, opt Options) *Manager {
	if opt.Log == nil {
		opt.Log = log.Default()
	}
	m := &Manager{opt: opt, hub: hub, cfg: cfg, frames: stream.NewFrameHub(), mjpegSync: make(chan struct{}, 1)}
	m.frames.OnCount = m.mjpegViewers
	m.removeCaptureFiles()
	m.publishState("waiting", "waiting for camera helper")
	return m
}

// Frames returns the hub of MJPEG fallback viewers. Its viewer count
// switches the helper's MJPEG encoder on and off.
func (m *Manager) Frames() *stream.FrameHub { return m.frames }

// Config returns the current configuration.
func (m *Manager) Config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) publishState(state, detail string) {
	ev := map[string]any{"state": state}
	if detail != "" {
		ev["detail"] = detail
	}
	m.hub.PublishSticky(stream.NewEvent(stream.EventState, ev))
}

// OnVideo handles video messages from the helper (passed to
// helper.Listener.Accept).
func (m *Manager) OnVideo(msg *proto.VideoMsg) {
	switch msg.Type {
	case proto.VideoAU:
		m.hub.Publish(stream.NewVideo(msg))
	case proto.VideoStreamStart:
		ev := map[string]any{}
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			m.opt.Log.Printf("camera: bad stream_start: %v", err)
		}
		m.hub.ResetSession()
		m.hub.PublishSticky(stream.NewEvent(stream.EventStream, ev))
		m.opt.Log.Printf("camera: stream_start %s", msg.Payload)
	case proto.VideoStreamEnd:
		m.opt.Log.Printf("camera: stream_end %s", msg.Payload)
	case proto.VideoJPEG:
		m.frames.Publish(msg.Payload)
	}
}

// mjpegViewers is the FrameHub.OnCount hook. When MJPEG viewers appear or
// all leave, it signals the session loop (RunSession), which starts or stops
// the helper's MJPEG encoder. Quick changes coalesce into one signal.
func (m *Manager) mjpegViewers(n int) {
	m.mu.Lock()
	want := n > 0
	changed := want != m.mjpegWanted
	m.mjpegWanted = want
	m.mu.Unlock()
	if changed {
		select {
		case m.mjpegSync <- struct{}{}:
		default: // a signal is already pending; syncMJPEG reads the latest state
		}
	}
}

// syncMJPEG tells the helper the current MJPEG state and settings.
func (m *Manager) syncMJPEG(ctx context.Context) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.syncMJPEGLocked(ctx)
}

func (m *Manager) syncMJPEGLocked(ctx context.Context) {
	m.mu.RLock()
	sess, want, cfg := m.sess, m.mjpegWanted, m.cfg
	m.mu.RUnlock()
	if sess == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p := proto.SetMJPEGParams{Enabled: want, Bitrate: cfg.MJPEG.Bitrate, Skip: cfg.MJPEG.Skip, FrameRate: EncoderFrameRate(cfg.Controls)}
	if err := sess.Call(cctx, proto.MethodSetMJPEG, p, nil); err != nil {
		m.opt.Log.Printf("camera: set_mjpeg %+v: %v", p, err)
		return
	}
	if want {
		m.opt.Log.Printf("camera: MJPEG fallback encoder on")
	} else {
		m.opt.Log.Printf("camera: MJPEG fallback encoder off")
	}
}

// RunSession drives one helper session until it ends: wait for hello, send
// the full configuration, then forward notifications.
func (m *Manager) RunSession(ctx context.Context, sess *helper.Session) error {
	defer func() {
		m.mu.Lock()
		if m.sess == sess {
			m.sess = nil
		}
		m.mu.Unlock()
	}()

	var hello proto.Hello
	select {
	case n, ok := <-sess.Notifications():
		if !ok {
			return fmt.Errorf("session ended before hello: %v", sess.Err())
		}
		if n.Method != proto.NoteHello {
			return fmt.Errorf("first notification is %q, want hello", n.Method)
		}
		if err := json.Unmarshal(n.Params, &hello); err != nil {
			return fmt.Errorf("bad hello: %w", err)
		}
	case <-time.After(60 * time.Second):
		return errors.New("no hello within 60 s")
	case <-ctx.Done():
		return ctx.Err()
	}
	if hello.Proto != proto.ProtoVersion {
		return fmt.Errorf("helper speaks protocol %d, server speaks %d", hello.Proto, proto.ProtoVersion)
	}
	m.opt.Log.Printf("camera: helper %s %s, camera %q, %d sensor modes, %d controls",
		hello.Impl, hello.ImplVersion, hello.Camera.Model, len(hello.Camera.SensorModes), len(hello.Camera.Controls))
	m.checkSensorMode(&hello)
	m.publishCamera(&hello)

	m.opMu.Lock()
	m.mu.Lock()
	m.sess, m.hello = sess, &hello
	cfg := m.cfg
	m.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	var res proto.ConfigureResult
	err := sess.Call(cctx, proto.MethodConfigure, ConfigureParams(cfg, &hello, m.opt.HFlip, m.opt.VFlip), &res)
	cancel()
	if err == nil && m.wantMJPEG() {
		m.syncMJPEGLocked(ctx) // a new helper starts without the MJPEG encoder
	}
	m.opMu.Unlock()
	if err != nil {
		m.publishState("error", "configure failed: "+err.Error())
		return fmt.Errorf("configure: %w", err)
	}
	m.opt.Log.Printf("camera: configured, session %d, applied %s", res.Session, res.Applied)

	for {
		select {
		case n, ok := <-sess.Notifications():
			if !ok {
				m.publishState("helper-restarting", fmt.Sprint(sess.Err()))
				return sess.Err()
			}
			m.handleNotification(n)
		case <-m.mjpegSync:
			m.syncMJPEG(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// publishCamera sends a sticky "camera" event with the camera model and the
// sensor's analogue gain limit (from libcamera's control ranges).
func (m *Manager) publishCamera(h *proto.Hello) {
	ev := map[string]any{"model": h.Camera.Model, "impl": h.Impl, "impl_version": h.ImplVersion}
	if max, ok := h.Camera.Controls["AnalogueGain"].MaxFloat(); ok {
		ev["sensor_max_gain"] = max
	}
	m.hub.PublishSticky(stream.NewEvent(stream.EventCamera, ev))
}

func (m *Manager) wantMJPEG() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mjpegWanted
}

func (m *Manager) checkSensorMode(h *proto.Hello) {
	if len(h.Camera.SensorModes) == 0 {
		return
	}
	for _, sm := range h.Camera.SensorModes {
		if sm.Size == SensorSize && sm.BitDepth == SensorBitDepth {
			return
		}
	}
	m.opt.Log.Printf("camera: WARNING: sensor mode %v %d-bit not reported by camera (modes: %+v)", SensorSize, SensorBitDepth, h.Camera.SensorModes)
}

func (m *Manager) handleNotification(n *proto.Notification) {
	switch n.Method {
	case proto.NoteState:
		var p proto.StateParams
		_ = json.Unmarshal(n.Params, &p)
		m.publishState(p.State, p.Detail)
	case proto.NoteMetadata:
		// Sticky: new viewers and GET /api/status get the latest values.
		m.hub.PublishSticky(stream.NewEvent(stream.EventMetadata, map[string]any{"data": n.Params}))
	case proto.NoteStats:
		m.hub.PublishSticky(stream.NewEvent(stream.EventStats, map[string]any{"data": n.Params}))
	case proto.NoteError:
		m.opt.Log.Printf("camera: helper error: %s", n.Params)
		m.hub.Publish(stream.NewEvent(stream.EventError, map[string]any{"data": n.Params}))
	default:
		m.opt.Log.Printf("camera: unknown notification %q", n.Method)
	}
}

// Apply merges a partial JSON config, validates it and applies it with the
// least disruptive helper call(s). Without a helper the config is only
// stored and applied at the next session.
func (m *Manager) Apply(ctx context.Context, partial []byte) (ApplyKind, Config, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	old := m.Config()
	next, err := Merge(old, partial)
	if err != nil {
		return ApplyNone, old, err
	}
	return m.applyLocked(ctx, old, next)
}

// Reset restores the default configuration.
func (m *Manager) Reset(ctx context.Context) (ApplyKind, Config, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.applyLocked(ctx, m.Config(), Defaults())
}

func (m *Manager) applyLocked(ctx context.Context, old, next Config) (ApplyKind, Config, error) {
	m.mu.RLock()
	sess, hello := m.sess, m.hello
	m.mu.RUnlock()
	if err := Validate(next, hello); err != nil {
		return ApplyNone, old, err
	}
	kind := Diff(old, next)
	if sess != nil && kind != ApplyNone {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := m.callApply(cctx, sess, hello, kind, old, next); err != nil {
			return ApplyNone, old, err
		}
	}
	m.mu.Lock()
	m.cfg = next
	m.mu.Unlock()
	// Restart the fallback encoder with new settings, or after a frame-rate
	// change (the helper gives the encoder its frame rate, fps / skip).
	if sess != nil && m.wantMJPEG() &&
		(old.MJPEG != next.MJPEG || FrameDurationUS(old.Controls) != FrameDurationUS(next.Controls)) {
		m.syncMJPEGLocked(ctx)
	}
	if err := SaveConfig(m.opt.ConfigPath, next); err != nil {
		m.opt.Log.Printf("camera: saving config: %v", err)
	}
	return kind, next, nil
}

func (m *Manager) callApply(ctx context.Context, sess *helper.Session, hello *proto.Hello, kind ApplyKind, old, next Config) error {
	switch kind {
	case ApplyReconfigure:
		var res proto.ConfigureResult
		return sess.Call(ctx, proto.MethodConfigure, ConfigureParams(next, hello, m.opt.HFlip, m.opt.VFlip), &res)
	case ApplyEncoder, ApplyLive:
		// Controls first (they may change the frame rate), then the encoder.
		changed := ChangedControls(ControlMap(old.Controls, hello), ControlMap(next.Controls, hello))
		if len(changed) > 0 {
			if err := sess.Call(ctx, proto.MethodSetControls, proto.SetControlsParams{Controls: changed}, nil); err != nil {
				return err
			}
		}
		if kind == ApplyEncoder {
			var res proto.SetEncoderResult
			return sess.Call(ctx, proto.MethodSetEncoder, proto.SetEncoderParams{
				Bitrate: next.Encoder.Bitrate, IPeriod: next.Encoder.GOP, FrameRate: EncoderFrameRate(next.Controls),
			}, &res)
		}
	}
	return nil
}

// Capture is a snapshot JPEG, held in memory.
type Capture struct {
	Name     string // download name: IMG_YYYYMMDD_HHMMSS.jpg
	JPEG     []byte
	Width    int
	Height   int
	TimingMS map[string]float64 // helper timings (capture, encode)
}

// Snapshot captures a JPEG from the live main stream. quality 0 means the
// configured quality. The helper writes the JPEG to CapturesDir; it is read
// into memory and the file is deleted at once, so nothing accumulates on the
// Pi.
func (m *Manager) Snapshot(ctx context.Context, quality int) (Capture, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	sess, cfg := m.sess, m.cfg
	m.mu.RUnlock()
	if sess == nil {
		return Capture{}, ErrNoCamera
	}
	if quality == 0 {
		quality = cfg.Still.Quality
	}
	if err := FieldByPath("still.quality").Check(float64(quality)); err != nil {
		return Capture{}, err
	}
	dir, err := filepath.Abs(m.opt.CapturesDir)
	if err != nil {
		return Capture{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Capture{}, err
	}
	stem := m.captureStem(time.Now())
	base := filepath.Join(dir, stem)   // the helper writes base.jpg (via base.jpg.tmp)
	defer os.Remove(base + ".jpg.tmp") // left behind if the helper failed mid-write
	defer os.Remove(base + ".jpg")

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var res proto.SnapshotResult
	if err := sess.Call(cctx, proto.MethodSnapshot, proto.SnapshotParams{Path: base, Quality: quality}, &res); err != nil {
		return Capture{}, err
	}
	jpeg, err := os.ReadFile(base + ".jpg") // our own path, not the one the helper reports
	if err != nil {
		return Capture{}, err
	}
	return Capture{Name: stem + ".jpg", JPEG: jpeg, Width: res.File.Width, Height: res.File.Height, TimingMS: res.TimingMS}, nil
}

// captureStem returns IMG_YYYYMMDD_HHMMSS, with _1, _2 … for further
// captures within the same second. Called with opMu held.
func (m *Manager) captureStem(t time.Time) string {
	stem := "IMG_" + t.Format("20060102_150405")
	if stem != m.lastStem {
		m.lastStem, m.nameSeq = stem, 0
		return stem
	}
	m.nameSeq++
	return fmt.Sprintf("%s_%d", stem, m.nameSeq)
}

// removeCaptureFiles deletes capture files left in CapturesDir by a crash.
// Only capture names are touched, so CapturesDir may be a shared directory.
func (m *Manager) removeCaptureFiles() {
	for _, pattern := range []string{"IMG_*.jpg", "IMG_*.jpg.tmp"} {
		files, _ := filepath.Glob(filepath.Join(m.opt.CapturesDir, pattern))
		for _, f := range files {
			if err := os.Remove(f); err == nil {
				m.opt.Log.Printf("camera: removed leftover capture %s", f)
			}
		}
	}
}

// Shutdown asks the helper to exit cleanly.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.RLock()
	sess := m.sess
	m.mu.RUnlock()
	if sess != nil {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = sess.Call(cctx, proto.MethodShutdown, nil, nil)
	}
}
