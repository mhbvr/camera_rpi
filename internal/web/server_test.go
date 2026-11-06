package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhbvr/camera_rpi/internal/camera"
	"github.com/mhbvr/camera_rpi/internal/proto"
	"github.com/mhbvr/camera_rpi/internal/stream"
)

func newTestServer(t *testing.T) (*httptest.Server, *stream.Hub, *camera.Manager) {
	hub := stream.NewHub(8)
	logger := log.New(io.Discard, "", 0)
	mgr := camera.NewManager(camera.Defaults(), hub, camera.Options{ConfigPath: filepath.Join(t.TempDir(), "c.json"), Log: logger})
	ts := httptest.NewServer(New(mgr, hub, nil, logger).Handler())
	t.Cleanup(ts.Close)
	return ts, hub, mgr
}

func TestStreamOverTLS(t *testing.T) {
	hub := stream.NewHub(8)
	logger := log.New(io.Discard, "", 0)
	mgr := camera.NewManager(camera.Defaults(), hub, camera.Options{ConfigPath: filepath.Join(t.TempDir(), "c.json"), Log: logger})
	ts := httptest.NewUnstartedServer(New(mgr, hub, nil, logger).Handler())
	ts.Config.ConnContext = ConnContext
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream", nil)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Errorf("want HTTP/2 over TLS, got %s", res.Proto)
	}
	h := make([]byte, stream.HeaderSize)
	if _, err := io.ReadFull(res.Body, h); err != nil || h[0] != stream.TypeEvent {
		t.Fatalf("first message: %v type %d", err, h[0])
	}
}

func TestIndexAndStatic(t *testing.T) {
	ts, _, _ := newTestServer(t)
	for _, p := range []string{"/", "/static/app.js", "/static/player.js", "/static/style.css"} {
		res, err := http.Get(ts.URL + p)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, err, res.Status)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if p == "/" && !strings.Contains(string(b), `data-path="controls.fps"`) {
			t.Fatal("index missing settings form")
		}
	}
	if res, _ := http.Get(ts.URL + "/nope"); res.StatusCode != 404 {
		t.Fatalf("unknown path: %v", res.Status)
	}
}

func TestConfigAPI(t *testing.T) {
	ts, _, _ := newTestServer(t)
	res, err := http.Post(ts.URL+"/api/config", "application/json", strings.NewReader(`{"controls":{"ev":1.5}}`))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("post: %v %v", err, res.Status)
	}
	var ar struct {
		Applied string        `json:"applied"`
		Config  camera.Config `json:"config"`
	}
	_ = json.NewDecoder(res.Body).Decode(&ar)
	if ar.Applied != "live" || ar.Config.Controls.EV != 1.5 {
		t.Fatalf("response %+v", ar)
	}
	res, _ = http.Post(ts.URL+"/api/config", "application/json", strings.NewReader(`{"controls":{"fps":0}}`))
	var e map[string]string
	_ = json.NewDecoder(res.Body).Decode(&e)
	if res.StatusCode != 400 || e["field"] != "controls.fps" {
		t.Fatalf("validation error: %v %v", res.Status, e)
	}
	res, _ = http.Post(ts.URL+"/api/config/reset", "application/json", nil)
	_ = json.NewDecoder(res.Body).Decode(&ar)
	if ar.Config.Controls.EV != 0 {
		t.Fatal("reset failed")
	}
}

func TestCaptureWithoutCamera(t *testing.T) {
	ts, _, _ := newTestServer(t)
	res, _ := http.Post(ts.URL+"/api/capture", "application/json", nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", res.Status)
	}
}

func TestStream(t *testing.T) {
	ts, hub, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	readMsg := func() (uint8, []byte) {
		h := make([]byte, stream.HeaderSize)
		if _, err := io.ReadFull(res.Body, h); err != nil {
			t.Fatal(err)
		}
		p := make([]byte, binary.BigEndian.Uint32(h[4:8]))
		if _, err := io.ReadFull(res.Body, p); err != nil {
			t.Fatal(err)
		}
		return h[0], p
	}
	// sticky state event first
	typ, p := readMsg()
	if typ != stream.TypeEvent || !strings.Contains(string(p), `"state"`) {
		t.Fatalf("first message type %d %s", typ, p)
	}
	// wait until the handler subscribed, then publish a keyframe
	for hub.Viewers() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	hub.Publish(stream.NewVideo(&proto.VideoMsg{Type: proto.VideoAU, Flags: proto.FlagKey, Payload: []byte{0, 0, 0, 1, 0x65}}))
	typ, p = readMsg()
	if typ != stream.TypeVideo || len(p) != 5 {
		t.Fatalf("video message type %d len %d", typ, len(p))
	}
}

func TestMJPEGEndpoint(t *testing.T) {
	hub := stream.NewHub(8)
	frames := stream.NewFrameHub()
	logger := log.New(io.Discard, "", 0)
	mgr := camera.NewManager(camera.Defaults(), hub, camera.Options{ConfigPath: filepath.Join(t.TempDir(), "c.json"), Log: logger, MJPEG: frames})
	ts := httptest.NewServer(New(mgr, hub, frames, logger).Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream.mjpeg", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	mt, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/x-mixed-replace" {
		t.Fatalf("content type %q %v", res.Header.Get("Content-Type"), err)
	}
	for frames.Viewers() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	jpeg := []byte{0xff, 0xd8, 1, 2, 3, 0xff, 0xd9}
	frames.Publish(jpeg)
	part, err := multipart.NewReader(res.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(part)
	if part.Header.Get("Content-Type") != "image/jpeg" || !bytes.Equal(got, jpeg) {
		t.Fatalf("part %v %x", part.Header, got)
	}
}

func TestStreamEventsOnly(t *testing.T) {
	ts, hub, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream?video=0", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	h := make([]byte, stream.HeaderSize)
	if _, err := io.ReadFull(res.Body, h); err != nil || h[0] != stream.TypeEvent { // sticky state
		t.Fatalf("first: %v %d", err, h[0])
	}
	if _, err := io.ReadFull(res.Body, make([]byte, binary.BigEndian.Uint32(h[4:8]))); err != nil {
		t.Fatal(err)
	}
	for hub.Viewers() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	hub.Publish(stream.NewVideo(&proto.VideoMsg{Type: proto.VideoAU, Flags: proto.FlagKey, Payload: []byte{0, 0, 0, 1, 0x65}}))
	hub.Publish(stream.NewEvent(map[string]any{"event": "metadata"}))
	if _, err := io.ReadFull(res.Body, h); err != nil || h[0] != stream.TypeEvent {
		t.Fatalf("events-only stream delivered type %d (%v)", h[0], err)
	}
}
