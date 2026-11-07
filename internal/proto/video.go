// Package proto implements the camera helper IPC protocol (docs/HELPER_API.md):
// length-prefixed video messages and JSON-RPC 2.0 control messages.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// VideoHeaderSize is the size of the binary header in front of every
// video.sock message.
const VideoHeaderSize = 32

// MaxVideoPayload guards against corrupt length fields.
const MaxVideoPayload = 8 << 20

// Video message types.
const (
	VideoAU          uint8 = 1 // one H.264 access unit, Annex-B
	VideoStreamStart uint8 = 2 // JSON StreamStart
	VideoStreamEnd   uint8 = 3 // JSON StreamEnd
	VideoJPEG        uint8 = 4 // one complete JPEG (MJPEG fallback)
)

// Video message flags.
const (
	FlagKey    uint8 = 1 << 0 // IDR frame
	FlagConfig uint8 = 1 << 1 // contains SPS/PPS
)

// VideoMsg is one message on video.sock.
type VideoMsg struct {
	Type    uint8
	Flags   uint8
	Seq     uint64
	PTSUS   uint64 // sensor timestamp, µs
	WallUS  uint64 // helper wall clock at hand-off, µs since Unix epoch
	Payload []byte
}

// Key reports whether the message is an IDR access unit.
func (m *VideoMsg) Key() bool { return m.Type == VideoAU && m.Flags&FlagKey != 0 }

// AppendHeader appends the 32-byte header for m to b.
func AppendHeader(b []byte, m *VideoMsg) []byte {
	var h [VideoHeaderSize]byte
	binary.BigEndian.PutUint32(h[0:4], uint32(len(m.Payload)))
	h[4] = m.Type
	h[5] = m.Flags
	binary.BigEndian.PutUint64(h[8:16], m.Seq)
	binary.BigEndian.PutUint64(h[16:24], m.PTSUS)
	binary.BigEndian.PutUint64(h[24:32], m.WallUS)
	return append(b, h[:]...)
}

// WriteVideoMsg writes header and payload in a single Write call.
func WriteVideoMsg(w io.Writer, m *VideoMsg) error {
	buf := make([]byte, 0, VideoHeaderSize+len(m.Payload))
	buf = AppendHeader(buf, m)
	buf = append(buf, m.Payload...)
	_, err := w.Write(buf)
	return err
}

// ErrPayloadTooLarge is returned for messages above MaxVideoPayload.
var ErrPayloadTooLarge = errors.New("proto: video payload too large")

// ReadVideoMsg reads one message. The payload is freshly allocated, so the
// caller may keep it.
func ReadVideoMsg(r io.Reader) (*VideoMsg, error) {
	var h [VideoHeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[0:4])
	if n > MaxVideoPayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, n)
	}
	m := &VideoMsg{
		Type:   h[4],
		Flags:  h[5],
		Seq:    binary.BigEndian.Uint64(h[8:16]),
		PTSUS:  binary.BigEndian.Uint64(h[16:24]),
		WallUS: binary.BigEndian.Uint64(h[24:32]),
	}
	m.Payload = make([]byte, n)
	if _, err := io.ReadFull(r, m.Payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return m, nil
}
