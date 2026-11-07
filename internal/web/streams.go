package web

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/mhbvr/camera_rpi/internal/stream"
)

// Long-running streaming responses (docs/REST_API.md "Video streams"):
//   /stream        H.264 + events, 16-byte framing (the bundled page)
//   /stream.h264   raw H.264 Annex-B
//   /api/events    events as Server-Sent Events
//   /stream.mjpeg  multipart JPEG (MJPEG fallback)

// writeTimeout ends a response whose client stopped reading.
const writeTimeout = 5 * time.Second

// streamWriter sends a streaming response: headers go out at once, and every
// write has a deadline and is flushed.
type streamWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startStream(w http.ResponseWriter, contentType string) *streamWriter {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // no buffering in reverse proxies
	w.WriteHeader(http.StatusOK)
	sw := &streamWriter{w: w, rc: http.NewResponseController(w)}
	_ = sw.rc.Flush()
	return sw
}

// write sends the chunks and flushes. It returns false when the client is
// gone or did not take the data within writeTimeout.
func (sw *streamWriter) write(chunks ...[]byte) bool {
	_ = sw.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	for _, c := range chunks {
		if _, err := sw.w.Write(c); err != nil {
			return false
		}
	}
	return sw.rc.Flush() == nil
}

// hubStream describes how one endpoint sends a hub subscription.
type hubStream struct {
	contentType string
	encode      func(*stream.Message) []byte // nil: skip the message
	keepalive   func() []byte                // nil: none
	idle        time.Duration                // keepalive after this long without data
}

// serveHub sends sub to the client until it disconnects, the subscription
// ends or a write fails. Messages queued together are written in one flush.
func (s *Server) serveHub(w http.ResponseWriter, r *http.Request, sub *stream.Sub, hs hubStream) {
	defer s.hub.Unsubscribe(sub)
	sw := startStream(w, hs.contentType)
	var tick <-chan time.Time
	if hs.keepalive != nil {
		t := time.NewTicker(hs.idle / 2)
		defer t.Stop()
		tick = t.C
	}
	last := time.Now()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.Ready():
			msgs, ok := sub.Take()
			var chunks [][]byte
			for _, m := range msgs {
				if b := hs.encode(m); b != nil {
					chunks = append(chunks, b)
				}
			}
			if len(chunks) > 0 {
				if !sw.write(chunks...) {
					return
				}
				last = time.Now()
			}
			if !ok {
				return
			}
		case <-tick:
			if time.Since(last) >= hs.idle {
				if !sw.write(hs.keepalive()) {
					return
				}
				last = time.Now()
			}
		}
	}
}

// handleStream serves H.264 and events in the 16-byte framing (/stream).
// ?video=0: events only (state, metadata, stats) for MJPEG fallback pages.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	var sub *stream.Sub
	if r.URL.Query().Get("video") == "0" {
		sub = s.hub.SubscribeEvents()
	} else {
		sub = s.hub.Subscribe()
		shrinkSendBuffer(r)
	}
	s.serveHub(w, r, sub, hubStream{
		contentType: "application/octet-stream",
		encode:      func(m *stream.Message) []byte { return m.Wire },
		keepalive:   func() []byte { return stream.Keepalive().Wire },
		idle:        2 * time.Second,
	})
}

// handleH264 serves the raw H.264 Annex-B elementary stream for players such
// as ffplay, mpv or VLC. It starts at a keyframe (SPS/PPS included) and,
// like /stream, skips to the next keyframe when the client falls behind.
func (s *Server) handleH264(w http.ResponseWriter, r *http.Request) {
	sub := s.hub.Subscribe()
	shrinkSendBuffer(r)
	s.serveHub(w, r, sub, hubStream{
		contentType: "video/h264",
		encode: func(m *stream.Message) []byte {
			if m.Type != stream.TypeVideo {
				return nil
			}
			return m.Payload()
		},
	})
}

// handleEvents serves the events of /stream (state, camera, stream,
// metadata, stats, error) as Server-Sent Events, one JSON object per "data:"
// line. The latest state, camera, stream, metadata and stats come first.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.serveHub(w, r, s.hub.SubscribeEvents(), hubStream{
		contentType: "text/event-stream",
		encode: func(m *stream.Message) []byte {
			// The payload is single-line JSON (encoding/json escapes newlines).
			return fmt.Appendf(nil, "data: %s\n\n", m.Payload())
		},
		keepalive: func() []byte { return []byte(": keepalive\n\n") },
		idle:      15 * time.Second,
	})
}

// mjpegBoundary separates the parts of the multipart MJPEG stream.
const mjpegBoundary = "camweb-frame"

// handleMJPEG serves the MJPEG fallback (ARCHITECTURE.md §5.3) as
// multipart/x-mixed-replace for an <img> element. While at least one
// client is connected, the helper runs a hardware MJPEG encoder; each
// client gets the newest frame (slow clients skip frames).
func (s *Server) handleMJPEG(w http.ResponseWriter, r *http.Request) {
	if s.frames == nil {
		http.NotFound(w, r)
		return
	}
	sub := s.frames.Subscribe()
	defer s.frames.Unsubscribe(sub)
	sw := startStream(w, "multipart/x-mixed-replace; boundary="+mjpegBoundary)
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-sub.C:
			if !ok {
				return
			}
			part := fmt.Appendf(nil, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", mjpegBoundary, len(frame))
			if !sw.write(part, frame, []byte("\r\n")) {
				return
			}
		}
	}
}

// shrinkSendBuffer keeps the kernel send buffer small for video clients, so
// a backlog stays in the hub queue, where it can be dropped to the next
// keyframe (ARCHITECTURE.md §4.2).
func shrinkSendBuffer(r *http.Request) {
	c, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok {
		return
	}
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(256 << 10)
	}
}
