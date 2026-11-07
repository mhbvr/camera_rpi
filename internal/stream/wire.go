// Package stream fans the encoded video out to browser viewers and defines
// the browser wire format (REST_API.md, /stream).
package stream

import (
	"encoding/binary"
	"encoding/json"
	"time"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

// HeaderSize is the size of the browser message header.
const HeaderSize = 16

// Browser message types.
const (
	TypeVideo     uint8 = 1
	TypeEvent     uint8 = 2
	TypeKeepalive uint8 = 3
)

// Browser message flags (same bits as proto flags).
const (
	FlagKey    = proto.FlagKey
	FlagConfig = proto.FlagConfig
)

// Event names: the "event" field of every event (docs/REST_API.md, Events).
// Sticky events (the latest one is replayed to new viewers and served by
// GET /api/status) are published with Hub.PublishSticky.
const (
	EventState    = "state"    // sticky: pipeline state
	EventCamera   = "camera"   // sticky: camera model and limits
	EventStream   = "stream"   // sticky: H.264 encoder (re)started
	EventMetadata = "metadata" // sticky: per-frame camera metadata
	EventStats    = "stats"    // sticky: helper frame counters
	EventError    = "error"    // asynchronous camera error
	EventDrop     = "drop"     // frames this viewer lost
)

// Message is one encoded browser message. Wire holds header+payload and is
// shared read-only between viewers.
type Message struct {
	Type  uint8
	Flags uint8
	Event string // event name, for TypeEvent
	Wire  []byte
}

// Key reports whether m is an IDR video frame.
func (m *Message) Key() bool { return m.Type == TypeVideo && m.Flags&FlagKey != 0 }

func encode(typ, flags uint8, ts uint64, payload []byte) *Message {
	w := make([]byte, HeaderSize+len(payload))
	w[0] = typ
	w[1] = flags
	binary.BigEndian.PutUint32(w[4:8], uint32(len(payload)))
	binary.BigEndian.PutUint64(w[8:16], ts)
	copy(w[HeaderSize:], payload)
	return &Message{Type: typ, Flags: flags, Wire: w}
}

// NewVideo builds a video message from a helper access unit. ts is the
// helper's wall-clock hand-off time.
func NewVideo(m *proto.VideoMsg) *Message {
	return encode(TypeVideo, m.Flags&(FlagKey|FlagConfig), m.WallUS, m.Payload)
}

// NewEvent builds an event message: the JSON object of fields plus
// "event": name. fields may be nil.
func NewEvent(name string, fields map[string]any) *Message {
	obj := map[string]any{}
	for k, v := range fields {
		obj[k] = v
	}
	obj["event"] = name
	b, err := json.Marshal(obj)
	if err != nil {
		b = []byte(`{"event":"error","message":"marshal failed"}`)
	}
	m := encode(TypeEvent, 0, uint64(time.Now().UnixMicro()), b)
	m.Event = name
	return m
}

// Keepalive returns an empty keepalive message.
func Keepalive() *Message { return encode(TypeKeepalive, 0, uint64(time.Now().UnixMicro()), nil) }
