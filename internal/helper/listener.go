package helper

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Socket file names inside the runtime directory.
const (
	CtlSocket   = "ctl.sock"
	VideoSocket = "video.sock"
)

// Listener owns ctl.sock and video.sock. The helper connects to ctl first,
// then video.
type Listener struct {
	dir        string
	ctl, video *net.UnixListener
	log        *log.Logger
}

// Listen creates the runtime directory and both sockets (mode 0600).
func Listen(dir string, logger *log.Logger) (*Listener, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Listener{dir: dir, log: logger}
	if len(l.VideoPath()) > 100 {
		return nil, fmt.Errorf("helper: socket path too long (%d bytes); use a shorter -runtime-dir", len(l.VideoPath()))
	}
	if l.ctl, err = listenUnix(l.CtlPath()); err != nil {
		return nil, err
	}
	if l.video, err = listenUnix(l.VideoPath()); err != nil {
		l.ctl.Close()
		return nil, err
	}
	return l, nil
}

func listenUnix(path string) (*net.UnixListener, error) {
	_ = os.Remove(path)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// CtlPath is the control socket path.
func (l *Listener) CtlPath() string { return filepath.Join(l.dir, CtlSocket) }

// VideoPath is the video socket path.
func (l *Listener) VideoPath() string { return filepath.Join(l.dir, VideoSocket) }

// Close removes both sockets.
func (l *Listener) Close() error {
	l.ctl.Close()
	l.video.Close()
	return nil
}

// Accept waits for the next helper: a ctl connection followed (within 10 s)
// by a video connection. When ctx ends, the listeners are closed and Accept
// returns ctx.Err(); ctx is the server's lifetime.
func (l *Listener) Accept(ctx context.Context, onVideo VideoHandler) (*Session, error) {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	for {
		ctl, err := l.ctl.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		_ = l.video.SetDeadline(time.Now().Add(10 * time.Second))
		video, err := l.video.Accept()
		if err != nil {
			ctl.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			l.log.Printf("helper: ctl connected but no video connection: %v", err)
			continue
		}
		return newSession(ctl, video, onVideo, l.log), nil
	}
}
