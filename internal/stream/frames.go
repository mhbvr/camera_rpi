package stream

import "sync"

// FrameHub distributes complete frames (JPEGs for the MJPEG fallback) to
// viewers with "latest frame wins" semantics: a viewer that is slower than
// the camera skips frames instead of falling behind. OnCount, if set, is
// called (without locks held) whenever the number of viewers changes, so
// the camera can start the MJPEG encoder only while someone watches.
type FrameHub struct {
	OnCount func(n int)

	mu     sync.Mutex
	subs   map[*FrameSub]struct{}
	closed bool
}

// FrameSub is one viewer. C holds at most the newest frame; it is closed
// when the hub closes.
type FrameSub struct {
	C chan []byte
}

// NewFrameHub returns an empty hub.
func NewFrameHub() *FrameHub { return &FrameHub{subs: map[*FrameSub]struct{}{}} }

// Subscribe adds a viewer.
func (h *FrameHub) Subscribe() *FrameSub {
	h.mu.Lock()
	s := &FrameSub{C: make(chan []byte, 1)}
	if h.closed {
		close(s.C)
		h.mu.Unlock()
		return s
	}
	h.subs[s] = struct{}{}
	n := len(h.subs)
	h.mu.Unlock()
	h.notify(n)
	return s
}

// Unsubscribe removes a viewer.
func (h *FrameHub) Unsubscribe(s *FrameSub) {
	h.mu.Lock()
	if _, ok := h.subs[s]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, s)
	n := len(h.subs)
	h.mu.Unlock()
	h.notify(n)
}

// Viewers returns the number of viewers.
func (h *FrameHub) Viewers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Publish hands frame to every viewer, replacing any frame it has not
// taken yet. It never blocks.
func (h *FrameHub) Publish(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.C <- frame:
		default:
			select { // drop the stale frame, keep the new one
			case <-s.C:
			default:
			}
			select {
			case s.C <- frame:
			default:
			}
		}
	}
}

// Close disconnects all viewers.
func (h *FrameHub) Close() {
	h.mu.Lock()
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		close(s.C)
	}
	h.mu.Unlock()
	h.notify(0)
}

func (h *FrameHub) notify(n int) {
	if h.OnCount != nil {
		h.OnCount(n)
	}
}
