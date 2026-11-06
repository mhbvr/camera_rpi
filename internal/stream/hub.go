package stream

import "sync"

// Hub distributes messages to subscribers. Publish never blocks: a viewer
// whose queue is full loses frames until the next keyframe (drop-to-key).
type Hub struct {
	mu       sync.Mutex
	subs     map[*Sub]struct{}
	queueLen int
	sticky   map[string]*Message // last event per key, replayed to new viewers
	order    []string
	closed   bool
}

// Sub is one viewer.
type Sub struct {
	C        chan *Message
	noVideo  bool // events only (MJPEG fallback pages)
	waitKey  bool // skip video until the next keyframe
	overflow bool // waitKey was caused by a full queue
	dropped  int  // frames dropped since the last report
	total    int
}

// NewHub returns a hub with per-viewer queues of queueLen messages.
func NewHub(queueLen int) *Hub {
	if queueLen < 2 {
		queueLen = 2
	}
	return &Hub{subs: map[*Sub]struct{}{}, queueLen: queueLen, sticky: map[string]*Message{}}
}

// Subscribe adds a viewer. It first receives the sticky events (current
// state and stream info), then video starting at the next keyframe.
func (h *Hub) Subscribe() *Sub { return h.subscribe(false) }

// SubscribeEvents adds a subscriber that receives events only, no video.
func (h *Hub) SubscribeEvents() *Sub { return h.subscribe(true) }

func (h *Hub) subscribe(noVideo bool) *Sub {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &Sub{C: make(chan *Message, h.queueLen+len(h.order)+1), waitKey: true, noVideo: noVideo}
	if h.closed {
		close(s.C)
		return s
	}
	for _, k := range h.order {
		s.C <- h.sticky[k]
	}
	h.subs[s] = struct{}{}
	return s
}

// Unsubscribe removes a viewer.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.C)
	}
}

// Viewers returns the number of subscribers.
func (h *Hub) Viewers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Publish delivers m to all viewers.
func (h *Hub) Publish(m *Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		h.deliver(s, m)
	}
}

// PublishSticky publishes an event and remembers it under key, so new
// viewers receive the latest one on subscribe.
func (h *Hub) PublishSticky(key string, m *Message) {
	h.mu.Lock()
	if _, ok := h.sticky[key]; !ok {
		h.order = append(h.order, key)
	}
	h.sticky[key] = m
	h.mu.Unlock()
	h.Publish(m)
}

// ResetSession makes every viewer wait for the next keyframe (called on a
// new encoder session).
func (h *Hub) ResetSession() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.waitKey = true
	}
}

// Close disconnects all viewers; later subscribers get a closed channel.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		close(s.C)
	}
}

func (h *Hub) deliver(s *Sub, m *Message) {
	if m.Type == TypeVideo && s.noVideo {
		return
	}
	if m.Type == TypeVideo {
		if s.waitKey {
			if !m.Key() {
				if s.overflow {
					s.dropped++
				}
				return
			}
			s.waitKey = false
			if s.overflow && s.dropped > 0 {
				s.overflow = false
				s.total += s.dropped
				ev := NewEvent(map[string]any{"event": "drop", "dropped": s.dropped, "total": s.total})
				s.dropped = 0
				select {
				case s.C <- ev:
				default:
				}
			}
		}
		select {
		case s.C <- m:
		default:
			s.waitKey = true
			s.overflow = true
			s.dropped++
		}
		return
	}
	select {
	case s.C <- m:
	default: // events are best effort
	}
}
