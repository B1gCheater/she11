//go:build !windows

// Package pipex runs one-shot commands over plain pipes: no pseudo terminal is
// created, and stdout/stderr stay separate.
package pipex

import (
	"fmt"
	"io"
	"os/exec"
	"syscall"
)

// Proc is one running process with separate output pipes.
type Proc struct {
	Stdout io.ReadCloser
	Stderr io.ReadCloser

	cmd *exec.Cmd
}

// Start spawns path with args, using env as the complete environment and cwd
// as the working directory when non-empty. path must already be absolute:
// on Android, resolving a bare name can trip seccomp (faccessat2).
//
// The child's stdin is the null device, so a command that reads stdin sees
// EOF instead of hanging.
func Start(path string, args []string, env []string, cwd string) (*Proc, error) {
	if path == "" {
		return nil, fmt.Errorf("pipex: empty executable path")
	}
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Dir = cwd
	// A new process group is what lets Kill reach grandchildren; closing the
	// process alone leaves them running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = nil // /dev/null: reads return EOF

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pipex: start: %w", err)
	}
	return &Proc{Stdout: stdout, Stderr: stderr, cmd: cmd}, nil
}

// Wait blocks until the process exits and returns its exit status. Callers
// must finish reading both pipes first: Wait closes them.
func (p *Proc) Wait() int {
	err := p.cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		return 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
	}
	return -1
}

// Kill terminates the whole process group.
func (p *Proc) Kill() {
	if p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

// Close releases the pipes. The process itself is reaped by Wait.
func (p *Proc) Close() {
	if p.Stdout != nil {
		_ = p.Stdout.Close()
	}
	if p.Stderr != nil {
		_ = p.Stderr.Close()
	}
}
