package proto

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestVideoRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []*VideoMsg{
		{Type: VideoAU, Flags: FlagKey | FlagConfig, Seq: 1, PTSUS: 2, WallUS: 3, Payload: []byte{0, 0, 0, 1, 0x67}},
		{Type: VideoStreamStart, Payload: []byte(`{"session":1}`)},
		{Type: VideoAU, Seq: 2, Payload: nil},
	}
	for _, m := range in {
		if err := WriteVideoMsg(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range in {
		got, err := ReadVideoMsg(&buf)
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if got.Type != want.Type || got.Flags != want.Flags || got.Seq != want.Seq ||
			got.PTSUS != want.PTSUS || got.WallUS != want.WallUS || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("msg %d: got %+v want %+v", i, got, want)
		}
	}
	if _, err := ReadVideoMsg(&buf); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestVideoTruncated(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteVideoMsg(&buf, &VideoMsg{Type: VideoAU, Payload: make([]byte, 100)})
	b := buf.Bytes()[:VideoHeaderSize+50]
	if _, err := ReadVideoMsg(bytes.NewReader(b)); err != io.ErrUnexpectedEOF {
		t.Fatalf("want ErrUnexpectedEOF, got %v", err)
	}
}

func TestVideoOversize(t *testing.T) {
	h := AppendHeader(nil, &VideoMsg{Type: VideoAU})
	h[0], h[1], h[2], h[3] = 0xff, 0xff, 0xff, 0xff
	if _, err := ReadVideoMsg(bytes.NewReader(h)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("want ErrPayloadTooLarge, got %v", err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		line           string
		resp, note, rq bool
	}{
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, true, false, false},
		{`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"bad","data":{"field":"x"}}}`, true, false, false},
		{`{"jsonrpc":"2.0","method":"hello","params":{"proto":1}}`, false, true, false},
		{`{"jsonrpc":"2.0","id":3,"method":"configure"}`, false, false, true},
	}
	for _, c := range cases {
		m, err := ParseIncoming([]byte(c.line))
		if err != nil {
			t.Fatal(err)
		}
		if m.IsResponse() != c.resp || m.IsNotification() != c.note || m.IsRequest() != c.rq {
			t.Errorf("%s: resp=%v note=%v req=%v", c.line, m.IsResponse(), m.IsNotification(), m.IsRequest())
		}
		if m.Error != nil && m.Error.Field() != "x" {
			t.Errorf("field = %q", m.Error.Field())
		}
	}
	if _, err := ParseIncoming([]byte(`{"jsonrpc":"1.0","id":1}`)); err == nil {
		t.Error("expected version error")
	}
}

func TestLineReader(t *testing.T) {
	lr := NewLineReader(strings.NewReader("a\nbb\n"))
	for _, want := range []string{"a", "bb"} {
		got, err := lr.ReadLine()
		if err != nil || string(got) != want {
			t.Fatalf("got %q %v want %q", got, err, want)
		}
	}
	if _, err := lr.ReadLine(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	long := strings.Repeat("x", MaxLine+10) + "\n"
	if _, err := NewLineReader(strings.NewReader(long)).ReadLine(); err != ErrLineTooLong {
		t.Fatalf("want ErrLineTooLong, got %v", err)
	}
	// A line bigger than the bufio buffer but under the limit works.
	mid := strings.Repeat("y", 200<<10)
	got, err := NewLineReader(strings.NewReader(mid + "\n")).ReadLine()
	if err != nil || len(got) != len(mid) {
		t.Fatalf("mid line: %d %v", len(got), err)
	}
}
