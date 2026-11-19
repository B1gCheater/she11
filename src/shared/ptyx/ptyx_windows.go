//go:build windows

// Package ptyx spawns shells attached to a ConPTY (Win10 1809+).
package ptyx

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// Session wraps a shell process + ConPTY + kill-on-close Job Object.
type Session struct {
	cpty      *conpty.ConPty
	job       windows.Handle
	pid       int
	exitC     chan struct{}
	code      int
	mu        sync.Mutex
	closeOnce sync.Once
}

// Start spawns <shell> with args attached to a new ConPTY.
func Start(shell string, args []string, env []string, cols, rows int) (*Session, error) {
	cmdline := buildCommandLine(shell, args)
	cpty, err := conpty.Start(cmdline, conpty.ConPtyDimensions(cols, rows), conpty.ConPtyEnv(env))
	if err != nil {
		return nil, err
	}
	s := &Session{cpty: cpty, pid: cpty.Pid(), exitC: make(chan struct{})}

	// Job Object: kills the whole process tree when we close/kill.
	job, err := attachJob(s.pid)
	if err != nil {
		_ = cpty.Close()
		return nil, fmt.Errorf("attach shell job: %w", err)
	}
	s.job = job

	go func() {
		code, _ := s.cpty.Wait(context.Background())
		s.code = int(code)
		close(s.exitC)
	}()
	return s, nil
}

func attachJob(pid int) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err == nil {
		var proc windows.Handle
		proc, err = windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
		if err == nil {
			err = windows.AssignProcessToJobObject(job, proc)
			_ = windows.CloseHandle(proc)
		}
	}
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func buildCommandLine(shell string, args []string) string {
	quote := func(s string) string {
		if strings.ContainsAny(s, " \t\"") {
			return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
		}
		return s
	}
	parts := append([]string{quote(shell)}, args...)
	return strings.Join(parts, " ")
}

// Read reads shell output.
func (s *Session) Read(p []byte) (int, error) { return s.cpty.Read(p) }

// Write writes shell input.
func (s *Session) Write(p []byte) (int, error) { return s.cpty.Write(p) }

// Resize updates the console size.
func (s *Session) Resize(cols, rows int) error { return s.cpty.Resize(cols, rows) }

// Wait blocks until the shell exits (single cpty.Wait owner).
func (s *Session) Wait() int {
	<-s.exitC
	return s.code
}

// Done fires when the process exited.
func (s *Session) Done() <-chan struct{} { return s.exitC }

// Kill terminates the job (entire process tree) and closes the console.
func (s *Session) Kill() {
	s.mu.Lock()
	if s.job != 0 {
		_ = windows.TerminateJobObject(s.job, 1)
	}
	s.mu.Unlock()
	s.Close()
}

// Close releases the job and console exactly once, after the Wait owner has
// collected the exit status. Closing a process handle during Wait is unsafe.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		<-s.exitC
		s.mu.Lock()
		if s.job != 0 {
			_ = windows.CloseHandle(s.job)
			s.job = 0
		}
		s.mu.Unlock()
		_ = s.cpty.Close()
	})
}
