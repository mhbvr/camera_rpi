package proto

import (
	"bufio"
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

func TestLineScanner(t *testing.T) {
	sc := NewLineScanner(strings.NewReader("a\nbb\n"))
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if sc.Err() != nil || len(got) != 2 || got[0] != "a" || got[1] != "bb" {
		t.Fatalf("lines %q, err %v", got, sc.Err())
	}
	long := strings.Repeat("x", MaxLine+1) + "\n"
	sc = NewLineScanner(strings.NewReader(long))
	if sc.Scan() || !errors.Is(sc.Err(), bufio.ErrTooLong) {
		t.Fatalf("want ErrTooLong, got %v", sc.Err())
	}
	mid := strings.Repeat("y", 100<<10) // longer than the initial buffer
	sc = NewLineScanner(strings.NewReader(mid + "\n"))
	if !sc.Scan() || sc.Text() != mid {
		t.Fatalf("100 KiB line: %v", sc.Err())
	}
}
