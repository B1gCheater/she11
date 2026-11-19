package commander

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"she11/src/shared/proto"
)

func pipeFrame(tag byte, payload string) []byte {
	var hdr [5]byte
	hdr[0] = tag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	return append(hdr[:], payload...)
}

func TestPipeCaptureSeparatesChannels(t *testing.T) {
	stream := bytes.Join([][]byte{
		pipeFrame(pipeTagStdout, "hello "),
		pipeFrame(pipeTagStderr, "bad "),
		pipeFrame(pipeTagStdout, "world"),
	}, nil)
	cap := &pipeCapture{}
	cap.readAll(bytes.NewReader(stream))
	merged, stdout, stderr, discarded := cap.snapshot()
	if merged != "hello bad world" {
		t.Fatalf("merged = %q, want arrival order", merged)
	}
	if stdout != "hello world" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "bad " {
		t.Fatalf("stderr = %q", stderr)
	}
	if discarded != 0 {
		t.Fatalf("discarded = %d, want 0", discarded)
	}
}

func TestPipeCaptureTornTailIsCountedNotShown(t *testing.T) {
	stream := pipeFrame(pipeTagStdout, "kept")
	stream = append(stream, 0x01, 0x00, 0x00) // header cut in half
	cap := &pipeCapture{}
	cap.readAll(bytes.NewReader(stream))
	merged, stdout, _, discarded := cap.snapshot()
	if merged != "kept" || stdout != "kept" {
		t.Fatalf("merged = %q stdout = %q, want the complete frame only", merged, stdout)
	}
	if discarded != 3 {
		t.Fatalf("discarded = %d, want the 3 torn bytes", discarded)
	}
}

func TestPipeCaptureRejectsBadRecords(t *testing.T) {
	cases := map[string][]byte{
		"unknown tag":   pipeFrame(0x09, "x"),
		"empty payload": pipeFrame(pipeTagStdout, ""),
		"oversized": func() []byte {
			b := pipeFrame(pipeTagStdout, "x")
			binary.BigEndian.PutUint32(b[1:], pipeFrameMax+1)
			return b
		}(),
	}
	for name, stream := range cases {
		cap := &pipeCapture{}
		cap.readAll(bytes.NewReader(stream))
		merged, _, _, discarded := cap.snapshot()
		if merged != "" || discarded == 0 {
			t.Fatalf("%s: merged = %q discarded = %d, want no output and a discarded count", name, merged, discarded)
		}
	}
}

func TestSelectExecBackend(t *testing.T) {
	c := &Commander{}
	allFeatures := map[string]bool{proto.FeatureRelayCaps: true, proto.FeatureOpenCwdEnv: true}
	oldRelay := map[string]bool{}
	clientCaps := map[string]bool{
		proto.CapExecPipe: true, proto.CapExecCwdEnv: true, proto.CapExecSplitStderr: true,
	}

	cases := []struct {
		name     string
		features map[string]bool
		caps     map[string]bool
		opts     execOptions
		wantBack string
		wantMode string
		wantCode string
	}{
		{"pipe by default", allFeatures, clientCaps, execOptions{}, execBackendPipe, proto.ModeExecPipe, ""},
		{"tty forces pty", allFeatures, clientCaps, execOptions{TTY: true}, execBackendPTY, proto.ModeExec, ""},
		{"old relay falls back", oldRelay, clientCaps, execOptions{}, execBackendPTY, proto.ModeExec, ""},
		{"old client falls back", allFeatures, map[string]bool{}, execOptions{}, execBackendPTY, proto.ModeExec, ""},
		{"cwd with pipe", allFeatures, clientCaps, execOptions{Cwd: "/tmp"}, execBackendPipe, proto.ModeExecPipe, ""},
		{"cwd with tty is rejected", allFeatures, clientCaps, execOptions{Cwd: "/tmp", TTY: true}, "", "", errCodeCapability},
		{"cwd without relay support is rejected", oldRelay, clientCaps, execOptions{Cwd: "/tmp"}, "", "", errCodeCapability},
		{"env without client cap is rejected", allFeatures, map[string]bool{proto.CapExecPipe: true},
			execOptions{Env: []string{"A=b"}}, "", "", errCodeCapability},
	}
	for _, tc := range cases {
		lc := &liveConn{features: tc.features}
		backend, mode, err := c.selectExecBackend(lc, tc.caps, tc.opts)
		if tc.wantCode != "" {
			if err == nil || errCodeOf(err) != tc.wantCode {
				t.Fatalf("%s: err = %v (code %q), want %q", tc.name, err, errCodeOf(err), tc.wantCode)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if backend != tc.wantBack || mode != tc.wantMode {
			t.Fatalf("%s: got (%s,%s), want (%s,%s)", tc.name, backend, mode, tc.wantBack, tc.wantMode)
		}
	}
}

func TestErrCodeOf(t *testing.T) {
	if got := errCodeOf(nil); got != errCodeNone {
		t.Fatalf("nil error code = %q", got)
	}
	if got := errCodeOf(codedErrorf(errCodeTargetAmbiguous, "two matches")); got != errCodeTargetAmbiguous {
		t.Fatalf("coded code = %q", got)
	}
	wrapped := fmt.Errorf("ctx: %w", codedErrorf(errCodeRelayDown, "down"))
	if got := errCodeOf(wrapped); got != errCodeRelayDown {
		t.Fatalf("wrapped code = %q", got)
	}
	if got := errCodeOf(&openError{code: proto.CodeClientOffline, reason: "offline"}); got != errCodeClientOffline {
		t.Fatalf("openError code = %q", got)
	}
	if got := errCodeOf(&openError{code: proto.CodeShellUnavailable, reason: "no shell"}); got != errCodeShellMissing {
		t.Fatalf("shell unavailable code = %q", got)
	}
	if got := errCodeOf(&openError{code: proto.CodeTimeout, reason: "timeout"}); got != errCodeOpenTimeout {
		t.Fatalf("open timeout code = %q", got)
	}
	if got := errCodeOf(errors.New("boom")); got != errCodeInternal {
		t.Fatalf("plain error code = %q", got)
	}
}

// TestExecStructuredCarriesEveryRequiredField keeps the declared outputSchema
// and the payload in sync, including on failure paths.
func TestExecStructuredCarriesEveryRequiredField(t *testing.T) {
	required := execOutputSchema["required"].([]string)
	check := func(name string, out map[string]interface{}) {
		for _, key := range required {
			if _, ok := out[key]; !ok {
				t.Fatalf("%s: required field %q missing", name, key)
			}
		}
	}
	check("nil result", execStructured(nil, false, 0))
	check("not started", execStructured(&execResult{State: execNotStarted, SafeToRetry: true,
		ErrorCode: errCodeTargetNotFound}, false, 0))
	stdout, stderr := "out\n", "err\n"
	check("pipe", execStructured(&execResult{State: execCompleted, Backend: execBackendPipe,
		Output: "out\nerr\n", Stdout: &stdout, Stderr: &stderr}, false, 0))
	check("pty", execStructured(&execResult{State: execCompleted, Backend: execBackendPTY,
		Output: "out\n"}, false, 0))

	out := execStructured(&execResult{State: execCompleted, Backend: execBackendPTY, Output: "x"}, false, 0)
	if out["stdout"] != nil || out["stderr"] != nil {
		t.Fatalf("pty backend must report null channels, got stdout=%v stderr=%v", out["stdout"], out["stderr"])
	}
	out = execStructured(&execResult{State: execCompleted, Backend: execBackendPipe,
		Output: "\x1b[31mred\x1b[0m", Stdout: &stdout, Stderr: &stderr}, false, 0)
	if strings.Contains(out["stdout"].(string), "\x1b") {
		t.Fatalf("stdout was not sanitized: %q", out["stdout"])
	}
	out = execStructured(&execResult{State: execCompleted, Backend: execBackendPipe,
		Output: "\x1b[31mred\x1b[0m", Stdout: &stdout, Stderr: &stderr}, true, 0)
	if !strings.Contains(out["output"].(string), "\x1b") {
		t.Fatalf("raw_output must preserve escape sequences: %q", out["output"])
	}
}

func TestMCPValidationRejectsBadExecArguments(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"cwd not a string", map[string]interface{}{"client_id": "id", "command": "x", "cwd": 5}},
		{"cwd empty", map[string]interface{}{"client_id": "id", "command": "x", "cwd": "  "}},
		{"env not an object", map[string]interface{}{"client_id": "id", "command": "x", "env": []string{"A=b"}}},
		{"env value not a string", map[string]interface{}{"client_id": "id", "command": "x", "env": map[string]interface{}{"A": 1}}},
		{"env key with equals", map[string]interface{}{"client_id": "id", "command": "x", "env": map[string]interface{}{"A=B": "v"}}},
		{"tty not a bool", map[string]interface{}{"client_id": "id", "command": "x", "tty": "yes"}},
	}
	for _, tc := range cases {
		if err := mcpValidateToolArgs("exec", tc.args); err == nil {
			t.Fatalf("%s: expected a rejection", tc.name)
		}
	}
	good := map[string]interface{}{"client_id": "id", "command": "x",
		"cwd": "/tmp", "env": map[string]interface{}{"A": "b"}, "tty": false}
	if err := mcpValidateToolArgs("exec", good); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
}
