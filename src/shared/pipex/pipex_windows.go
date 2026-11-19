//go:build windows

// Package pipex runs one-shot commands over plain pipes: no pseudo console is
// created, so repeated execs do not exercise the ConPTY handle-retention path,
// and stdout/stderr stay separate.
package pipex

import (
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// spawnMu serializes process creation while our child-side pipe handles are
// marked inheritable. CreateProcess duplicates every inheritable handle of the
// caller, so two overlapping spawns would each inherit the other's pipes and
// keep them from reaching EOF.
var spawnMu sync.Mutex

// Proc is one running process with separate output pipes.
type Proc struct {
	Stdout io.ReadCloser
	Stderr io.ReadCloser

	hProc  windows.Handle
	job    windows.Handle
	mu     sync.Mutex
	waited bool
	killed bool
	code   int
}

// Start spawns path with args, using env as the complete environment and cwd
// as the working directory when non-empty. The child's stdin is an empty pipe,
// so a command that reads stdin sees EOF instead of hanging.
func Start(path string, args []string, env []string, cwd string) (proc *Proc, err error) {
	spawnMu.Lock()
	defer spawnMu.Unlock()

	if path == "" {
		return nil, fmt.Errorf("pipex: empty executable path")
	}
	envp, err := envBlock(env)
	if err != nil {
		return nil, err
	}
	argv := append([]string{path}, args...)
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return nil, fmt.Errorf("pipex: command line: %w", err)
	}
	var cwdPtr *uint16
	if cwd != "" {
		if cwdPtr, err = windows.UTF16PtrFromString(cwd); err != nil {
			return nil, fmt.Errorf("pipex: cwd: %w", err)
		}
	}

	// stdin is the NUL device: any read returns EOF immediately, so a command
	// that reads stdin cannot hang, and there is no write end to leak.
	nul, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer nul.Close()
	if err := windows.SetHandleInformation(windows.Handle(nul.Fd()),
		windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return nil, fmt.Errorf("pipex: inherit stdin: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}
	// Only the ends the child uses may be inherited.
	child := []*os.File{stdoutW, stderrW}
	for _, f := range child {
		if err := windows.SetHandleInformation(windows.Handle(f.Fd()),
			windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			closeAll(append(child, stdoutR, stderrR)...)
			return nil, fmt.Errorf("pipex: inherit handle: %w", err)
		}
	}

	si := windows.StartupInfo{
		Cb:        uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:     windows.STARTF_USESTDHANDLES,
		StdInput:  windows.Handle(nul.Fd()),
		StdOutput: windows.Handle(stdoutW.Fd()),
		StdErr:    windows.Handle(stderrW.Fd()),
	}
	var pi windows.ProcessInformation
	// CREATE_SUSPENDED closes the race between process creation and job
	// assignment: the process cannot spawn a child before it is resumed.
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW)
	createErr := windows.CreateProcess(nil, cmdline, nil, nil, true, flags,
		envp, cwdPtr, &si, &pi)
	// The parent must drop the child ends whatever happened, or the read ends
	// would never see EOF.
	stdoutW.Close()
	stderrW.Close()
	if createErr != nil {
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("pipex: create process: %w", createErr)
	}

	job, jobErr := newJob(pi.Process)
	if jobErr != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Process)
		_ = windows.CloseHandle(pi.Thread)
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("pipex: job object: %w", jobErr)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
		_ = windows.CloseHandle(job)
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("pipex: resume thread: %w", err)
	}
	_ = windows.CloseHandle(pi.Thread)
	return &Proc{Stdout: stdoutR, Stderr: stderrR, hProc: pi.Process, job: job}, nil
}

// newJob creates a kill-on-close job and assigns the (still suspended)
// process to it, so the whole tree dies with this handle.
func newJob(proc windows.Handle) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func envBlock(env []string) (*uint16, error) {
	if len(env) == 0 {
		return nil, nil // inherit the client's environment
	}
	var buf []uint16
	for _, kv := range env {
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			return nil, fmt.Errorf("pipex: environment entry %q: %w", kv, err)
		}
		buf = append(buf, u...)
	}
	buf = append(buf, 0)
	return &buf[0], nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// Wait blocks until the process exits and returns its exit status. The job
// handle stays open until Close; closing it also kills any surviving child.
func (p *Proc) Wait() int {
	p.mu.Lock()
	if p.waited {
		code := p.code
		p.mu.Unlock()
		return code
	}
	p.waited = true
	p.mu.Unlock()

	_, _ = windows.WaitForSingleObject(p.hProc, windows.INFINITE)
	var code uint32
	_ = windows.GetExitCodeProcess(p.hProc, &code)
	p.mu.Lock()
	p.code = int(code)
	p.mu.Unlock()
	_ = windows.CloseHandle(p.hProc)
	p.mu.Lock()
	p.hProc = 0
	p.mu.Unlock()
	return int(code)
}

// Kill terminates the whole process tree.
func (p *Proc) Kill() {
	p.mu.Lock()
	if p.killed {
		p.mu.Unlock()
		return
	}
	p.killed = true
	job, h := p.job, p.hProc
	p.mu.Unlock()
	if job != 0 {
		_ = windows.TerminateJobObject(job, 1)
	}
	if h != 0 {
		_ = windows.TerminateProcess(h, 1)
	}
}

// Close releases the pipes and the job object. Closing a job whose
// kill-on-close limit is set terminates any process still inside it, which is
// the intended cleanup for a one-shot command that left a grandchild behind.
func (p *Proc) Close() {
	if p.Stdout != nil {
		_ = p.Stdout.Close()
	}
	if p.Stderr != nil {
		_ = p.Stderr.Close()
	}
	p.mu.Lock()
	job := p.job
	p.job = 0
	p.mu.Unlock()
	if job != 0 {
		_ = windows.CloseHandle(job)
	}
}
