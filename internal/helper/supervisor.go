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

// Supervisor runs the helper process and restarts it with backoff
// (1 s, 2 s, 4 s … 30 s; reset after 60 s of healthy running).
type Supervisor struct {
	Args []string // command and arguments
	Env  []string // extra environment ("K=V"), appended to os.Environ
	Log  *log.Logger

	mu  sync.Mutex
	cmd *exec.Cmd
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

func (s *Supervisor) runOnce(ctx context.Context) error {
	cmd := exec.Command(s.Args[0], s.Args[1:]...)
	cmd.Env = append(cmd.Environ(), s.Env...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // one pipe for both; stdout is unused by the protocol
	if err := cmd.Start(); err != nil {
		return err
	}
	s.Log.Printf("helper: started pid %d: %v", cmd.Process.Pid, s.Args)
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()

	logDone := make(chan struct{})
	go func() { s.pipeLog(out); close(logDone) }()

	waitDone := make(chan error, 1)
	go func() {
		<-logDone
		waitDone <- cmd.Wait()
	}()
	select {
	case err = <-waitDone:
	case <-ctx.Done():
		s.terminate(cmd, waitDone)
		err = ctx.Err()
	}
	s.mu.Lock()
	s.cmd = nil
	s.mu.Unlock()
	return err
}

func (s *Supervisor) pipeLog(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		s.Log.Printf("[helper] %s", sc.Text())
	}
}

// Kill terminates the current helper process (it will be restarted by Run).
func (s *Supervisor) Kill() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			time.Sleep(3 * time.Second)
			_ = cmd.Process.Kill() // no-op error if already gone
		}()
	}
}

func (s *Supervisor) terminate(cmd *exec.Cmd, waitDone <-chan error) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-waitDone
	}
}
