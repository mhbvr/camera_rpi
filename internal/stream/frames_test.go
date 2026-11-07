package stream

import (
	"sync"
	"testing"
)

func TestFrameHubLatestWins(t *testing.T) {
	var mu sync.Mutex
	var counts []int
	h := NewFrameHub()
	h.OnCount = func(n int) { mu.Lock(); counts = append(counts, n); mu.Unlock() }
	s := h.Subscribe()
	h.Publish([]byte("a"))
	h.Publish([]byte("b")) // replaces "a" without blocking
	if got := string(<-s.C); got != "b" {
		t.Fatalf("got %q, want newest frame", got)
	}
	s2 := h.Subscribe()
	h.Unsubscribe(s)
	h.Unsubscribe(s) // idempotent
	h.Unsubscribe(s2)
	mu.Lock()
	defer mu.Unlock()
	if len(counts) != 4 || counts[0] != 1 || counts[1] != 2 || counts[2] != 1 || counts[3] != 0 {
		t.Fatalf("counts %v", counts)
	}
}

func TestFrameHubClose(t *testing.T) {
	h := NewFrameHub()
	s := h.Subscribe()
	h.Close()
	if _, ok := <-s.C; ok {
		t.Fatal("channel should be closed")
	}
	if _, ok := <-h.Subscribe().C; ok {
		t.Fatal("subscribe after close should be closed")
	}
}

func TestEventsOnlySubscriber(t *testing.T) {
	h := NewHub(8)
	s := h.SubscribeEvents()
	h.Publish(video(true, 1))
	h.Publish(NewEvent("x", nil))
	got := drain(s)
	if len(got) != 1 || got[0].Type != TypeEvent {
		t.Fatalf("events-only subscriber got %d messages", len(got))
	}
}
