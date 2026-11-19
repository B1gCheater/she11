//go:build !windows

// Package ptyx spawns shells attached to a real PTY (tmux/vim-capable).
package ptyx

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// Session wraps a shell process + controlling PTY.
type Session struct {
	f         *os.File
	cmd       *exec.Cmd
	pid       int
	waitErr   error
	exitC     chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Start spawns <shell> with args attached to a new PTY.
func Start(shell string, args []string, env []string, cols, rows int) (*Session, error) {
	cmd := exec.Command(shell, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	ws := &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols), X: uint16(cols * 8), Y: uint16(rows * 16)}
	f, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return nil, err
	}
	s := &Session{f: f, cmd: cmd, pid: cmd.Process.Pid, exitC: make(chan struct{})}
	go func() { s.waitErr = cmd.Wait(); close(s.exitC) }()
	return s, nil
}

// Read reads shell output.
func (s *Session) Read(p []byte) (int, error) { return s.f.Read(p) }

// Write writes shell input.
func (s *Session) Write(p []byte) (int, error) { return s.f.Write(p) }

// Resize updates the PTY window size.
func (s *Session) Resize(cols, rows int) error {
	ws := &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols), X: uint16(cols * 8), Y: uint16(rows * 16)}
	return pty.Setsize(s.f, ws)
}

// Wait blocks until the shell exits; returns its exit code.
func (s *Session) Wait() int {
	<-s.exitC
	if s.cmd.ProcessState == nil {
		return -1
	}
	return s.cmd.ProcessState.ExitCode()
}

// Done fires when the process exited.
func (s *Session) Done() <-chan struct{} { return s.exitC }

// Kill terminates the whole process group (no orphans).
func (s *Session) Kill() {
	_ = syscall.Kill(-s.pid, syscall.SIGKILL)
}

// Close releases the PTY master.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		// The shell may have exited while leaving background children alive.
		s.Kill()
		s.closeErr = s.f.Close()
	})
	return s.closeErr
}

var _ io.ReadWriteCloser = (*Session)(nil)
