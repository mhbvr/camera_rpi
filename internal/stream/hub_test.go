package stream

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

func video(key bool, seq uint64) *Message {
	f := uint8(0)
	if key {
		f = proto.FlagKey
	}
	return NewVideo(&proto.VideoMsg{Type: proto.VideoAU, Flags: f, Seq: seq, WallUS: 42, Payload: []byte{byte(seq)}})
}

func drain(s *Sub) []*Message {
	msgs, _ := s.Take()
	return msgs
}

func TestWireFormat(t *testing.T) {
	m := NewVideo(&proto.VideoMsg{Type: proto.VideoAU, Flags: proto.FlagKey | proto.FlagConfig, WallUS: 123, Payload: []byte{1, 2, 3}})
	w := m.Wire
	if len(w) != HeaderSize+3 || w[0] != TypeVideo || w[1] != 3 ||
		binary.BigEndian.Uint32(w[4:8]) != 3 || binary.BigEndian.Uint64(w[8:16]) != 123 {
		t.Fatalf("bad wire %x", w)
	}
}

func TestNewViewerStartsAtKey(t *testing.T) {
	h := NewHub(10)
	s := h.Subscribe()
	h.Publish(video(false, 1))
	h.Publish(video(true, 2))
	h.Publish(video(false, 3))
	got := drain(s)
	if len(got) != 2 || !got[0].Key() {
		t.Fatalf("got %d msgs, first key=%v", len(got), len(got) > 0 && got[0].Key())
	}
}

func TestSlowViewerDropsToKey(t *testing.T) {
	h := NewHub(3)
	s := h.Subscribe()
	h.Publish(video(true, 0))
	for i := 1; i <= 10; i++ { // overflows the queue, never blocks
		h.Publish(video(false, uint64(i)))
	}
	drain(s)
	h.Publish(video(false, 11)) // still waiting for key
	if got := drain(s); len(got) != 0 {
		t.Fatalf("expected nothing before key, got %d", len(got))
	}
	h.Publish(video(true, 12))
	got := drain(s)
	if len(got) != 2 || got[0].Type != TypeEvent || !got[1].Key() {
		t.Fatalf("expected drop event + key, got %d", len(got))
	}
	var ev map[string]any
	_ = json.Unmarshal(got[0].Wire[HeaderSize:], &ev)
	if ev["event"] != "drop" || ev["dropped"].(float64) < 1 {
		t.Fatalf("bad drop event %v", ev)
	}
}

func TestStickyAndReset(t *testing.T) {
	h := NewHub(10)
	h.PublishSticky(NewEvent("state", map[string]any{"state": "a"}))
	h.PublishSticky(NewEvent("state", map[string]any{"state": "b"}))
	s := h.Subscribe()
	got := drain(s)
	if len(got) != 1 {
		t.Fatalf("want 1 sticky, got %d", len(got))
	}
	h.Publish(video(true, 1))
	h.ResetSession()
	h.Publish(video(false, 2))
	if got := drain(s); len(got) != 1 {
		t.Fatalf("after reset want only the first key, got %d", len(got))
	}
}

func TestCloseAndUnsubscribe(t *testing.T) {
	h := NewHub(4)
	a, b := h.Subscribe(), h.Subscribe()
	h.Unsubscribe(a)
	h.Unsubscribe(a) // idempotent
	h.Close()
	<-b.Ready()
	if _, ok := b.Take(); ok {
		t.Fatal("b should be closed")
	}
	c := h.Subscribe()
	<-c.Ready()
	if _, ok := c.Take(); ok {
		t.Fatal("subscribe after close should be closed")
	}
}

func TestEventsSurviveFullVideoQueue(t *testing.T) {
	h := NewHub(2)
	s := h.Subscribe()
	h.Publish(video(true, 0))
	for i := 1; i <= 5; i++ {
		h.Publish(video(false, uint64(i)))
	}
	h.Publish(NewEvent("state", map[string]any{"state": "reconfiguring"}))
	got := drain(s)
	if len(got) != 3 || got[2].Type != TypeEvent {
		t.Fatalf("want 2 frames + the state event, got %d messages", len(got))
	}
}

func TestStuckViewerIsClosed(t *testing.T) {
	h := NewHub(2)
	s := h.Subscribe()
	for i := 0; i <= maxQueuedEvents; i++ {
		h.Publish(NewEvent("stats", nil))
	}
	msgs, ok := s.Take()
	if len(msgs) != maxQueuedEvents {
		t.Fatalf("queued %d events", len(msgs))
	}
	if _, ok = s.Take(); ok {
		t.Fatal("a viewer that never reads should be closed")
	}
}
