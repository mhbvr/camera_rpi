package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
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
	ts := httptest.NewServer(New(mgr, hub, logger).Handler())
	t.Cleanup(ts.Close)
	return ts, hub, mgr
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
