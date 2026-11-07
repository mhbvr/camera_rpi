package helper

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// stopTimeout is how long the helper gets to exit after SIGTERM before it is
// killed.
const stopTimeout = 3 * time.Second

// Supervisor runs the helper process and restarts it with backoff
// (1 s, 2 s, 4 s … 30 s; reset after 60 s of healthy running).
type Supervisor struct {
	Args []string // command and arguments
	Env  []string // extra environment ("K=V"), appended to os.Environ
	Log  *log.Logger

	mu   sync.Mutex
	stop context.CancelFunc // stops the current run; nil between runs
}

// Run blocks until ctx is cancelled, keeping the helper running.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 60*time.Second {
			backoff = time.Second
		}
		s.Log.Printf("helper: exited (%v); restarting in %v", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// runOnce starts the helper and waits for it to exit. When ctx ends or Kill
// is called, the helper gets SIGTERM and, after stopTimeout, SIGKILL.
func (s *Supervisor) runOnce(ctx context.Context) error {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(runCtx, s.Args[0], s.Args[1:]...)
	cmd.Env = append(cmd.Environ(), s.Env...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopTimeout
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // one pipe for both; stdout is unused by the protocol
	if err := cmd.Start(); err != nil {
		return err
	}
	s.Log.Printf("helper: started pid %d: %v", cmd.Process.Pid, s.Args)
	s.setStop(stop)
	defer s.setStop(nil)

	s.pipeLog(out) // until the helper exits and the pipe closes
	return cmd.Wait()
}

func (s *Supervisor) setStop(stop context.CancelFunc) {
	s.mu.Lock()
	s.stop = stop
	s.mu.Unlock()
}

func (s *Supervisor) pipeLog(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		s.Log.Printf("[helper] %s", sc.Text())
	}
}

// Kill stops the current helper process; Run starts a new one.
func (s *Supervisor) Kill() {
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}
