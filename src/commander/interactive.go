// Package commander: interactive shell (raw mode, resize, exit code).
package commander

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"she11/src/shared/proto"
)

// StartShell opens an interactive PTY session. Returns process exit code
// (commander semantics: 100+n for remote exit codes).
func (c *Commander) StartShell(servers []ServerConf, target, shellType, forceProto string) int {
	lc, clientID, err := c.pickTarget(servers, target, forceProto)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 3
	}
	defer lc.conn.CloseWithError(0, "done")

	cols, rows := 80, 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		cols, rows = w, h
	}
	if shellType == "" {
		shellType = c.cfg.General.ShellType
	}
	o := proto.Open{
		ClientID:  clientID,
		Shell:     shellType,
		Mode:      "pty",
		Term:      interactiveTerm(),
		ColorTerm: interactiveColorTerm(),
		Cols:      cols,
		Rows:      rows,
		Operator:  "cli",
	}
	stm, sid, err := c.openSession(lc, o)
	if err != nil {
		if oe, ok := err.(*openError); ok {
			fmt.Fprintln(os.Stderr, "open failed:", oe.reason)
			return exitCodeFor(oe.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer stm.Close()

	restore, err := prepareTerminal()
	if err != nil {
		_ = lc.ctl.Send(proto.TClose, proto.CloseMsg{SessionID: sid})
		fmt.Fprintln(os.Stderr, "error preparing terminal:", err)
		return 1
	}
	var cleanupOnce sync.Once
	output := &terminalOutput{w: os.Stdout}
	cleanup := func() { cleanupOnce.Do(func() { output.finish(); restore() }) }
	defer cleanup()

	var remoteCode atomic.Int64
	remoteCode.Store(-1)

	// control reader: RESIZE pass-through is server-side; we await EXIT.
	exitCh := make(chan int, 1)
	go func() {
		for {
			env, err := lc.ctl.Recv()
			if err != nil {
				exitCh <- -1
				return
			}
			if env.Type == proto.TExit {
				m, _ := proto.Decode[proto.ExitMsg](env)
				exitCh <- m.Code
				return
			}
		}
	}()

	outDone := make(chan struct{})
	go func() { // stdin -> stream
		_, _ = io.Copy(stm, os.Stdin)
		stm.Close()
	}()
	go func() { // stream -> stdout
		defer close(outDone)
		_, _ = io.Copy(output, stm)
	}()
	stopResize := make(chan struct{})
	go resizePoller(lc, sid, stopResize)

	// signal -> send to remote? raw mode delivers Ctrl-C as byte anyway.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	var code int
	select {
	case code = <-exitCh:
		if code < 0 { // connection dropped without EXIT
			code = -1
		}
	case <-sigCh:
		code = -1
	case <-outDone:
		// The data stream ended without an EXIT frame: the relay or client
		// vanished. Give the control path a short grace for a racing EXIT,
		// then stop instead of blocking in raw mode forever. Ctrl-C there is
		// just a byte and would be written into a dead stream.
		select {
		case code = <-exitCh:
			if code < 0 {
				code = -1
			}
		case <-time.After(500 * time.Millisecond):
			code = -1
		}
	}
	close(stopResize)
	stm.Close()
	select {
	case <-outDone:
	case <-time.After(time.Second):
	}
	cleanup()
	if code < 0 {
		fmt.Fprintln(os.Stderr, "session ended without an exit status (connection lost)")
		return 1
	}
	return 100 + code
}

// Exec runs a one-shot command and returns its exit code (prints output).
func (c *Commander) Exec(servers []ServerConf, target, shellType, command, forceProto string) int {
	res, err := c.execCapture(servers, target, shellType, execOptions{
		Command: command, ProtoKind: forceProto, Timeout: c.timeout})
	if err != nil {
		// A timeout or a lost connection can still have produced output.
		// Printing it is the difference between "nothing happened" and "it got
		// this far", which decides whether re-running is safe.
		if res != nil && res.Output != "" {
			fmt.Print(res.Output)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		if oe, ok := err.(*openError); ok {
			return exitCodeFor(oe.code)
		}
		return 1
	}
	fmt.Print(res.Output)
	if res.ExitCode == nil {
		return 1
	}
	return 100 + *res.ExitCode
}

// execOptions is one one-shot attempt. Cwd and Env are only honored by the
// non-PTY backend; TTY forces the legacy PTY path.
type execOptions struct {
	Command   string
	Cwd       string
	Env       []string // KEY=VALUE
	TTY       bool
	ProtoKind string
	Timeout   time.Duration
}

// ExecCapture runs a one-shot command and reports its full outcome.
// timeout bounds the wait for the EXIT frame after the session is open; it is
// not a total deadline covering target discovery, connection and OPEN.
func (c *Commander) ExecCapture(servers []ServerConf, target, shellType, command string, timeout time.Duration) (*execResult, error) {
	return c.execCapture(servers, target, shellType, execOptions{Command: command, Timeout: timeout})
}

// selectExecBackend decides, before anything is submitted, whether the command
// can use the non-PTY path. Both halves matter: a client that advertises the
// cap through a relay that cannot forward it is not usable, and an old client
// would treat exec_pipe as "interactive" rather than rejecting it.
func (c *Commander) selectExecBackend(lc *liveConn, caps map[string]bool, opts execOptions) (backend, mode string, err error) {
	pipeCapable := caps[proto.CapExecPipe] && lc.hasFeature(proto.FeatureRelayCaps)
	if opts.TTY || !pipeCapable {
		if opts.Cwd != "" || len(opts.Env) > 0 {
			return "", "", codedErrorf(errCodeCapability,
				"cwd/env require the non-PTY backend (tty=false); client exec_pipe=%v, relay relay_caps=%v, tty requested=%v. Nothing was submitted; retry without cwd/env or upgrade the client and relay",
				caps[proto.CapExecPipe], lc.hasFeature(proto.FeatureRelayCaps), opts.TTY)
		}
		return execBackendPTY, proto.ModeExec, nil
	}
	if opts.Cwd != "" || len(opts.Env) > 0 {
		if !caps[proto.CapExecCwdEnv] || !lc.hasFeature(proto.FeatureOpenCwdEnv) {
			return "", "", codedErrorf(errCodeCapability,
				"cwd/env require client exec_cwd_env=%v and relay relay_open_cwd_env=%v; an old relay drops these fields silently, so nothing was submitted",
				caps[proto.CapExecCwdEnv], lc.hasFeature(proto.FeatureOpenCwdEnv))
		}
	}
	return execBackendPipe, proto.ModeExecPipe, nil
}

func (c *Commander) execCapture(servers []ServerConf, target, shellType string, opts execOptions) (*execResult, error) {
	start := time.Now()
	command := opts.Command
	timeout := opts.Timeout
	// Resolve the shell before any failure is reported, so an unsuccessful
	// attempt still says which shell it would have used.
	if shellType == "" {
		shellType = c.cfg.General.ShellType
	}
	if timeout <= 0 {
		timeout = c.timeout
	}
	// Nothing reached the client yet, so a retry cannot double a side effect.
	notStarted := func(err error) (*execResult, error) {
		return &execResult{State: execNotStarted, Shell: shellType, ClientID: target,
			DurationMs: time.Since(start).Milliseconds(), Error: err.Error(),
			ErrorCode: errCodeOf(err), SafeToRetry: true}, err
	}

	lc, clientID, caps, err := c.pickTargetInfo(servers, target, opts.ProtoKind)
	if err != nil {
		return notStarted(err)
	}
	defer lc.conn.CloseWithError(0, "done")

	backend, mode, err := c.selectExecBackend(lc, caps, opts)
	if err != nil {
		return notStarted(err)
	}

	o := proto.Open{
		ClientID: clientID,
		Shell:    shellType,
		Mode:     mode,
		Command:  command,
		Cols:     200,
		Rows:     50,
		Operator: "mcp",
	}
	if mode == proto.ModeExecPipe {
		o.Cwd, o.Env = opts.Cwd, opts.Env
	}
	stm, sid, err := c.openSession(lc, o)
	if err != nil {
		return notStarted(err)
	}
	var closeOnce sync.Once
	closeStream := func() { closeOnce.Do(func() { _ = stm.Close() }) }
	defer closeStream()

	// Past this point the command IS running on the client: the relay answers
	// open_result only after session_accept, which the client sends after a
	// successful spawn. So every failure below leaves the outcome unknown, and
	// an unknown outcome is never evidence that retrying is safe.
	base := execResult{Shell: shellType, ClientID: clientID, SessionID: sid, Backend: backend}

	codeCh := make(chan int, 1)
	go func() {
		for {
			env, err := lc.ctl.Recv()
			if err != nil {
				codeCh <- -1
				return
			}
			if env.Type == proto.TExit {
				m, _ := proto.Decode[proto.ExitMsg](env)
				codeCh <- m.Code
				return
			}
		}
	}()
	piped := backend == execBackendPipe
	var buf execOutputBuffer
	pipeCap := &pipeCapture{}
	outCh := make(chan struct{})
	go func() {
		defer close(outCh)
		if piped {
			pipeCap.readAll(stm)
		} else {
			_, _ = io.Copy(&buf, stm)
		}
	}()
	// fill copies whatever arrived into the result. The merged output stays the
	// compatibility field; the per-channel views exist only for the pipe
	// backend, where they are real rather than fabricated.
	fill := func(res *execResult) {
		if !piped {
			res.Output, res.Discarded = buf.Snapshot()
			return
		}
		merged, stdout, stderr, discarded := pipeCap.snapshot()
		res.Output, res.Discarded = merged, discarded
		res.Stdout, res.Stderr = &stdout, &stderr
	}

	finish := func(raw int) (*execResult, error) {
		res := base
		res.DurationMs = time.Since(start).Milliseconds()
		if raw < 0 {
			res.State = execLost
			fill(&res)
			res.Error = "connection closed before command exit status"
			res.ErrorCode = errCodeLost
			return &res, fmt.Errorf("%s", res.Error)
		}
		// Drain residual output; the client closes the stream right after EXIT.
		drained := true
		select {
		case <-outCh:
		case <-time.After(execDrainGrace):
			drained = false
		}
		code := raw
		res.State = execCompleted
		res.ExitCode = &code
		fill(&res)
		if !drained {
			res.Error = "timed out draining command output after exit"
			res.ErrorCode = errCodeDrainTimeout
			return &res, fmt.Errorf("%s", res.Error)
		}
		return &res, nil
	}

	select {
	case raw := <-codeCh:
		return finish(raw)
	case <-outCh:
		// The stream closed before EXIT. Wait briefly for the control frame
		// (the client sends EXIT just before closing), then report a lost
		// session instead of sitting here until timeout_ms.
		select {
		case raw := <-codeCh:
			return finish(raw)
		case <-time.After(execExitGrace):
			res := base
			res.DurationMs = time.Since(start).Milliseconds()
			res.State = execLost
			fill(&res)
			res.Error = "connection closed before command exit status"
			res.ErrorCode = errCodeLost
			return &res, fmt.Errorf("%s", res.Error)
		}
	case <-time.After(timeout):
		// Closing the data stream also tears down the relay, while CLOSE
		// explicitly asks the client to stop the process before disconnecting.
		stop := boundControl(lc, c.timeout)
		_ = lc.ctl.Send(proto.TClose, proto.CloseMsg{SessionID: sid})
		stop()
		closeStream()
		// Let in-flight bytes land, then report what did arrive. Dropping them
		// used to make a timeout indistinguishable from "never ran", which
		// invites a blind retry of a command that may already have had effects.
		select {
		case <-outCh:
		case <-time.After(execTimeoutDrain):
		}
		res := base
		res.State = execTimedOut
		res.DurationMs = time.Since(start).Milliseconds()
		fill(&res)
		res.Error = fmt.Sprintf("exec timed out after %s", timeout)
		res.ErrorCode = errCodeExecTimeout
		return &res, fmt.Errorf("%s", res.Error)
	}
}

// Execution states. They keep three questions apart: did the tool call work
// (the returned error), did the command run at all (State), and how did the
// process end (ExitCode, nil when unknown).
const (
	execNotStarted = "not_started" // never submitted to a client; safe to retry
	execCompleted  = "completed"   // ran to completion; exit code known
	execTimedOut   = "timed_out"   // submitted, then killed locally; outcome unknown
	execLost       = "lost"        // submitted, connection lost; outcome unknown
)

const (
	execOutputLimit = 1 << 20
	// execDrainGrace bounds the post-EXIT read on the normal path.
	execDrainGrace = 3 * time.Second
	// execTimeoutDrain is shorter: the caller already gave up waiting, and this
	// only collects bytes that were on their way.
	execTimeoutDrain = 300 * time.Millisecond
	// execExitGrace is how long a closed data stream waits for a racing EXIT
	// control frame before the attempt is reported as lost.
	execExitGrace = 500 * time.Millisecond
)

// execResult is the outcome of one one-shot command attempt.
//
// Output holds the captured bytes exactly as they arrived, including the
// terminal control sequences the PTY backend adds. Sanitizing is a
// presentation choice left to each caller, so the interactive CLI keeps its
// colors while MCP can spend less of an AI's context.
type execResult struct {
	State     string `json:"execution_state"`
	ExitCode  *int   `json:"exit_code"`
	Shell     string `json:"shell"`
	ClientID  string `json:"client_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Backend is "pipe" or "pty". Only the pipe backend produces real
	// per-channel views; under a PTY the OS merges both streams.
	Backend     string  `json:"exec_backend,omitempty"`
	Output      string  `json:"output"`
	Stdout      *string `json:"stdout,omitempty"`
	Stderr      *string `json:"stderr,omitempty"`
	Discarded   int64   `json:"discarded_bytes,omitempty"`
	DurationMs  int64   `json:"duration_ms"`
	Error       string  `json:"error,omitempty"`
	ErrorCode   string  `json:"error_code,omitempty"`
	SafeToRetry bool    `json:"safe_to_retry"`
}

// execOutputBuffer retains the newest output within a memory cap. The
// data-copy goroutine writes it while a timed-out caller snapshots it
// concurrently, so every access is locked.
type execOutputBuffer struct {
	mu        sync.Mutex
	data      []byte
	discarded int64
}

func (b *execOutputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if n >= execOutputLimit {
		b.discarded += int64(len(b.data) + n - execOutputLimit)
		b.data = append(b.data[:0], p[n-execOutputLimit:]...)
		return n, nil
	}
	if overflow := len(b.data) + n - execOutputLimit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.discarded += int64(overflow)
	}
	b.data = append(b.data, p...)
	return n, nil
}

// Snapshot copies out the buffered bytes plus the count the cap discarded. It
// reports truncation separately instead of inlining a marker, so callers can
// put it in a structured field rather than in the text an AI must parse.
func (b *execOutputBuffer) Snapshot() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return string(out), b.discarded
}

func (b *execOutputBuffer) String() string {
	out, discarded := b.Snapshot()
	if discarded > 0 {
		return fmt.Sprintf("[output truncated: %d bytes discarded]\n", discarded) + out
	}
	return out
}

func exitCodeFor(code int) int {
	switch code {
	case proto.CodeClientOffline:
		return 4
	case proto.CodeAuthzDenied, proto.CodeClientDenied, proto.CodeBadMagic, proto.CodeBadPassword:
		return 6
	case proto.CodeShellUnavailable, proto.CodeNoShell:
		return 7
	case proto.CodeTimeout:
		return 8
	case proto.CodeSessionLimit:
		return 6
	default:
		return 1
	}
}

func resizePoller(lc *liveConn, sid string, stop chan struct{}) {
	var lastW, lastH int
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			w, h, err := term.GetSize(int(os.Stdout.Fd()))
			if err != nil || (w == lastW && h == lastH) {
				continue
			}
			lastW, lastH = w, h
			_ = lc.ctl.Send(proto.TResize, proto.Resize{SessionID: sid, Cols: w, Rows: h})
		}
	}
}

var _ = runtime.GOOS
