package stream

import (
	"encoding/json"
	"sync"
)

// maxQueuedEvents bounds the events queued for one viewer. A viewer that
// reaches it is not reading at all; it is disconnected (it reconnects and
// gets the current state from the sticky events).
const maxQueuedEvents = 256

// Hub distributes video and events to subscribers. Publish never blocks.
//
// Video: a viewer whose queue holds queueLen frames loses frames until the
// next keyframe (drop-to-keyframe), then gets a "drop" event. Events are never
// dropped: they are small and rare, and a lost "state" event would leave a
// viewer showing the wrong state.
type Hub struct {
	mu       sync.Mutex
	subs     map[*Sub]struct{}
	queueLen int                 // video frames queued per viewer
	sticky   map[string]*Message // last event per key, replayed to new viewers
	order    []string
	closed   bool
}

// Sub is one viewer. Wait for Ready, then Take the queued messages.
type Sub struct {
	noVideo bool // events only (MJPEG fallback pages, /api/events)
	ready   chan struct{}

	mu       sync.Mutex
	queue    []*Message
	videos   int  // video frames in queue
	events   int  // events in queue
	closed   bool // no more messages after the queued ones
	waitKey  bool // skip video until the next keyframe
	overflow bool // waitKey was caused by a full queue
	dropped  int  // frames dropped since the last report
	total    int
}

// NewHub returns a hub that queues up to queueLen video frames per viewer.
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
	s := &Sub{noVideo: noVideo, waitKey: true, ready: make(chan struct{}, 1)}
	if h.closed {
		s.close()
		return s
	}
	for _, k := range h.order {
		s.add(h.sticky[k], false)
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
		s.close()
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
		s.deliver(m, h.queueLen)
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

// Payload returns the message payload (the bytes after the header).
func (m *Message) Payload() []byte { return m.Wire[HeaderSize:] }

// StickyEvents returns the JSON of the latest sticky event per key (for
// GET /api/status).
func (h *Hub) StickyEvents() map[string]json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]json.RawMessage, len(h.sticky))
	for k, m := range h.sticky {
		out[k] = json.RawMessage(m.Payload())
	}
	return out
}

// ResetSession makes every viewer wait for the next keyframe (called on a
// new encoder session).
func (h *Hub) ResetSession() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.mu.Lock()
		s.waitKey = true
		s.mu.Unlock()
	}
}

// Close disconnects all viewers; later subscribers are closed at once.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		s.close()
	}
}

// Ready is signalled when messages are queued or the subscription closed.
func (s *Sub) Ready() <-chan struct{} { return s.ready }

// Take returns the queued messages, oldest first. ok is false once the
// subscription is closed and everything queued has been taken.
func (s *Sub) Take() (msgs []*Message, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs, s.queue, s.videos, s.events = s.queue, nil, 0, 0
	return msgs, len(msgs) > 0 || !s.closed
}

func (s *Sub) deliver(m *Message, queueLen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if m.Type != TypeVideo {
		s.add(m, true)
		return
	}
	if s.noVideo {
		return
	}
	if s.waitKey {
		if !m.Key() {
			if s.overflow {
				s.dropped++
			}
			return
		}
		s.waitKey = false
		if s.overflow {
			s.overflow = false
			s.total += s.dropped
			s.add(NewEvent(map[string]any{"event": "drop", "dropped": s.dropped, "total": s.total}), true)
			s.dropped = 0
		}
	}
	if s.videos >= queueLen {
		s.waitKey, s.overflow = true, true
		s.dropped++
		return
	}
	s.add(m, true)
}

// add queues m and signals Ready. Called with s.mu held (or before s is
// shared). With limit, a viewer with too many queued events is closed.
func (s *Sub) add(m *Message, limit bool) {
	if m.Type == TypeVideo {
		s.videos++
	} else {
		if limit && s.events >= maxQueuedEvents {
			s.closed = true
			s.signal()
			return
		}
		s.events++
	}
	s.queue = append(s.queue, m)
	s.signal()
}

func (s *Sub) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.signal()
}

func (s *Sub) signal() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}
