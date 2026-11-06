// Package helper manages the camera helper process and its IPC session
// (docs/PROTOCOL.md): two Unix sockets, JSON-RPC 2.0 on ctl.sock and
// length-prefixed video on video.sock.
package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhbvr/camera_rpi/internal/proto"
)

// VideoHandler receives every video message. It is called from the video
// reader goroutine and must not block for long.
type VideoHandler func(*proto.VideoMsg)

// ErrClosed is returned by Call after the session ended.
var ErrClosed = errors.New("helper: session closed")

// Session is one connected helper (ctl + video connections).
type Session struct {
	ctl, video net.Conn
	log        *log.Logger

	wmu    sync.Mutex
	nextID atomic.Int64

	pmu     sync.Mutex
	pending map[int64]chan *proto.Incoming

	notes chan *proto.Notification

	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func newSession(ctl, video net.Conn, onVideo VideoHandler, logger *log.Logger) *Session {
	s := &Session{
		ctl:     ctl,
		video:   video,
		log:     logger,
		pending: map[int64]chan *proto.Incoming{},
		notes:   make(chan *proto.Notification, 256),
		done:    make(chan struct{}),
	}
	go s.readCtl()
	go s.readVideo(onVideo)
	return s
}

// Notifications delivers helper notifications (hello, state, metadata, …).
// It is closed when the session ends.
func (s *Session) Notifications() <-chan *proto.Notification { return s.notes }

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended.
func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// Close ends the session and closes both connections.
func (s *Session) Close() { s.fail(ErrClosed) }

func (s *Session) fail(err error) {
	s.closeOnce.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.done)
		s.ctl.Close()
		s.video.Close()
	})
}

// Call sends a request and waits for its response. A JSON-RPC error is
// returned as *proto.RPCError. result may be nil.
func (s *Session) Call(ctx context.Context, method string, params, result any) error {
	select {
	case <-s.done:
		return ErrClosed
	default:
	}
	id := s.nextID.Add(1)
	ch := make(chan *proto.Incoming, 1)
	s.pmu.Lock()
	s.pending[id] = ch
	s.pmu.Unlock()
	defer func() {
		s.pmu.Lock()
		delete(s.pending, id)
		s.pmu.Unlock()
	}()

	line, err := proto.MarshalLine(proto.Request{JSONRPC: proto.Version, ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	s.wmu.Lock()
	_ = s.ctl.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = s.ctl.Write(line)
	s.wmu.Unlock()
	if err != nil {
		s.fail(fmt.Errorf("ctl write: %w", err))
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("helper: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("helper: %s: %w", method, ctx.Err())
	case <-s.done:
		return ErrClosed
	}
}

func (s *Session) readCtl() {
	defer close(s.notes)
	lr := proto.NewLineReader(s.ctl)
	for {
		line, err := lr.ReadLine()
		if err != nil {
			s.fail(fmt.Errorf("ctl read: %w", err))
			return
		}
		if len(line) == 0 {
			continue
		}
		m, err := proto.ParseIncoming(line)
		if err != nil {
			s.log.Printf("helper: bad ctl message: %v", err)
			continue
		}
		switch {
		case m.IsResponse():
			s.pmu.Lock()
			ch := s.pending[*m.ID]
			s.pmu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.IsNotification():
			n := &proto.Notification{JSONRPC: m.JSONRPC, Method: m.Method, Params: m.Params}
			select {
			case s.notes <- n:
			case <-s.done:
				return
			default:
				if m.Method != proto.NoteMetadata && m.Method != proto.NoteStats {
					s.log.Printf("helper: notification queue full, dropping %s", m.Method)
				}
			}
		default:
			s.log.Printf("helper: unexpected ctl message: %s", line)
		}
	}
}

func (s *Session) readVideo(onVideo VideoHandler) {
	br := bufio.NewReaderSize(s.video, 512<<10)
	for {
		m, err := proto.ReadVideoMsg(br)
		if err != nil {
			s.fail(fmt.Errorf("video read: %w", err))
			return
		}
		if onVideo != nil {
			onVideo(m)
		}
	}
}
