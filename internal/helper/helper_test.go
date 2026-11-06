package helper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

// fakeHelper connects like a real helper and answers requests.
func fakeHelper(t *testing.T, l *Listener) (ctl, video net.Conn) {
	t.Helper()
	var err error
	if ctl, err = net.Dial("unix", l.CtlPath()); err != nil {
		t.Fatal(err)
	}
	if video, err = net.Dial("unix", l.VideoPath()); err != nil {
		t.Fatal(err)
	}
	go func() {
		lr := proto.NewLineReader(ctl)
		write := func(v any) {
			b, _ := proto.MarshalLine(v)
			ctl.Write(b)
		}
		write(proto.Notification{JSONRPC: "2.0", Method: "hello", Params: json.RawMessage(`{"proto":1}`)})
		for {
			line, err := lr.ReadLine()
			if err != nil {
				return
			}
			m, _ := proto.ParseIncoming(line)
			switch m.Method {
			case "ping_slow":
				go func(id int64) {
					time.Sleep(100 * time.Millisecond)
					write(proto.Response{JSONRPC: "2.0", ID: &id, Result: json.RawMessage(`{"slow":true}`)})
				}(*m.ID)
			case "echo":
				write(proto.Response{JSONRPC: "2.0", ID: m.ID, Result: m.Params})
			default:
				write(proto.Response{JSONRPC: "2.0", ID: m.ID, Error: &proto.RPCError{Code: proto.CodeMethodNotFound, Message: "nope"}})
			}
		}
	}()
	return ctl, video
}

func TestSession(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	l, err := Listen(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	videoCh := make(chan *proto.VideoMsg, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessCh := make(chan *Session, 1)
	go func() {
		s, err := l.Accept(ctx, func(m *proto.VideoMsg) { videoCh <- m })
		if err != nil {
			t.Error(err)
		}
		sessCh <- s
	}()
	ctl, video := fakeHelper(t, l)
	defer ctl.Close()
	s := <-sessCh

	// hello notification
	select {
	case n := <-s.Notifications():
		if n.Method != "hello" {
			t.Fatalf("first notification %q", n.Method)
		}
	case <-ctx.Done():
		t.Fatal("no hello")
	}

	// out-of-order responses: slow call started first, echo returns first
	slowDone := make(chan error, 1)
	go func() {
		var r map[string]bool
		err := s.Call(ctx, "ping_slow", nil, &r)
		if err == nil && !r["slow"] {
			err = errors.New("bad slow result")
		}
		slowDone <- err
	}()
	var echo map[string]int
	if err := s.Call(ctx, "echo", map[string]int{"a": 1}, &echo); err != nil || echo["a"] != 1 {
		t.Fatalf("echo: %v %v", echo, err)
	}
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}

	// JSON-RPC error
	err = s.Call(ctx, "missing", nil, nil)
	var rpcErr *proto.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != proto.CodeMethodNotFound {
		t.Fatalf("want method not found, got %v", err)
	}

	// video
	if err := proto.WriteVideoMsg(video, &proto.VideoMsg{Type: proto.VideoAU, Flags: proto.FlagKey, Payload: []byte{1, 2}}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-videoCh:
		if !m.Key() || len(m.Payload) != 2 {
			t.Fatalf("bad video msg %+v", m)
		}
	case <-ctx.Done():
		t.Fatal("no video")
	}

	// closing the helper's video side ends the session
	video.Close()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session not done after video close")
	}
	if err := s.Call(ctx, "echo", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}
