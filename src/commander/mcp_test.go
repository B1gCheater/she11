package commander

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"she11/src/shared/frame"
	"she11/src/shared/proto"
)

func TestMCPHTTPProtocolAndOrigin(t *testing.T) {
	c := &Commander{}
	handler := c.mcpHTTPHandler("test-token")
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	tests := []struct {
		name, method, body, origin, version, auth string
		status, rpcCode                           int
	}{
		{name: "request without Origin", body: ping, auth: "Bearer test-token", status: 200},
		{name: "local Origin", body: ping, origin: "http://localhost:8899", auth: "Bearer test-token", status: 200},
		{name: "loopback IPv6 Origin", body: ping, origin: "http://[::1]:8899", auth: "Bearer test-token", status: 200},
		{name: "remote Origin", body: ping, origin: "https://attacker.example", auth: "Bearer test-token", status: 403, rpcCode: -32000},
		{name: "deceptive Origin", body: ping, origin: "http://localhost.attacker.example", auth: "Bearer test-token", status: 403, rpcCode: -32000},
		{name: "opaque Origin", body: ping, origin: "null", auth: "Bearer test-token", status: 403, rpcCode: -32000},
		{name: "notification", body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, auth: "Bearer test-token", status: 202},
		{name: "unknown notification", body: `{"jsonrpc":"2.0","method":"notifications/unknown"}`, auth: "Bearer test-token", status: 202},
		{name: "multiline JSON", body: "{\n\"jsonrpc\":\"2.0\",\n\"id\":1,\n\"method\":\"ping\"\n}", auth: "Bearer test-token", status: 200},
		{name: "malformed JSON", body: "{", auth: "Bearer test-token", status: 400, rpcCode: -32700},
		{name: "invalid request", body: `{"jsonrpc":"1.0","id":1,"method":"ping"}`, auth: "Bearer test-token", status: 400, rpcCode: -32600},
		{name: "invalid id", body: `{"jsonrpc":"2.0","id":{},"method":"ping"}`, auth: "Bearer test-token", status: 400, rpcCode: -32600},
		{name: "unsupported version", body: ping, version: "2099-01-01", auth: "Bearer test-token", status: 400, rpcCode: -32600},
		{name: "supported version", body: ping, version: mcpProtocolVersion, auth: "Bearer test-token", status: 200},
		{name: "missing authorization", body: ping, status: 401, rpcCode: -32001},
		{name: "unsupported GET", method: http.MethodGet, auth: "Bearer test-token", status: 405, rpcCode: -32600},
		{name: "local preflight", method: http.MethodOptions, origin: "http://127.0.0.1:8899", status: 204},
		{name: "remote preflight", method: http.MethodOptions, origin: "http://attacker.example", status: 403, rpcCode: -32000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, "/mcp", strings.NewReader(tt.body))
			req.Header.Set("Authorization", tt.auth)
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("MCP-Protocol-Version", tt.version)
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", res.Code, tt.status, res.Body.String())
			}
			if tt.status == 202 || tt.status == 204 {
				if res.Body.Len() != 0 {
					t.Fatalf("notification/preflight body = %q", res.Body.String())
				}
				return
			}
			var rpc rpcResponse
			if err := json.Unmarshal(res.Body.Bytes(), &rpc); err != nil {
				t.Fatalf("response is not JSON-RPC: %v: %s", err, res.Body.String())
			}
			if tt.rpcCode != 0 && (rpc.Error == nil || rpc.Error.Code != tt.rpcCode) {
				t.Fatalf("RPC error = %+v, want %d", rpc.Error, tt.rpcCode)
			}
			if tt.status == 200 && rpc.Error != nil {
				t.Fatalf("unexpected RPC error: %+v", rpc.Error)
			}
			if tt.origin != "" && tt.status == 200 && res.Header().Get("Access-Control-Allow-Origin") != tt.origin {
				t.Fatal("allowed Origin was not reflected")
			}
			if tt.status == 403 && res.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("rejected Origin received CORS permission")
			}
		})
	}
}

func TestMCPStdioMalformedInputDoesNotHideErrors(t *testing.T) {
	var output bytes.Buffer
	(&Commander{}).mcpServe(strings.NewReader("{\n"+
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`+"\n"+
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n"), &output)
	decoder := json.NewDecoder(&output)
	var malformed, ping rpcResponse
	if err := decoder.Decode(&malformed); err != nil || malformed.Error == nil || malformed.Error.Code != -32700 {
		t.Fatalf("malformed input response = %+v, error = %v", malformed, err)
	}
	if err := decoder.Decode(&ping); err != nil || string(ping.ID) != "2" || ping.Error != nil {
		t.Fatalf("next valid request response = %+v, error = %v", ping, err)
	}
	if decoder.Decode(&rpcResponse{}) != io.EOF {
		t.Fatal("notification produced an extra response")
	}
}

func TestMCPRequiredArgumentsRejectedBeforeExecution(t *testing.T) {
	tests := []struct {
		name string
		args map[string]interface{}
	}{
		{"exec", map[string]interface{}{"client_id": "abc"}},
		{"exec", map[string]interface{}{"client_id": "abc", "command": "echo x", "timeout_ms": 1.5}},
		{"exec", map[string]interface{}{"client_id": "abc", "command": "echo x", "timeout_ms": "1000"}},
		{"start_shell", map[string]interface{}{"client_id": " "}},
		{"shell_write", map[string]interface{}{"session_id": "abc"}},
		{"shell_read", map[string]interface{}{"session_id": 10}},
		{"shell_close", nil},
	}
	for _, tt := range tests {
		params, _ := json.Marshal(map[string]interface{}{"name": tt.name, "arguments": tt.args})
		resp := (&Commander{}).mcpHandle(&rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
		if resp.Error == nil || resp.Error.Code != -32602 {
			t.Errorf("%s(%v) = %+v; want invalid params", tt.name, tt.args, resp)
		}
	}
	if err := mcpValidateToolArgs("shell_write", map[string]interface{}{"session_id": "abc", "input": ""}); err != nil {
		t.Fatalf("empty but present input should be valid: %v", err)
	}
}

func TestMCPUnreadBufferBoundedAndConsumed(t *testing.T) {
	s := &mcpSession{sid: "test", exitedCh: make(chan struct{})}
	writer := &mcpBufferAdapter{s: s}
	payload := bytes.Repeat([]byte("x"), mcpMaxUnreadBytes+123)
	_, _ = writer.Write(payload)
	_, _ = writer.Write([]byte("TAIL"))
	if len(s.buf) != mcpMaxUnreadBytes {
		t.Fatalf("unread bytes = %d, want bound %d", len(s.buf), mcpMaxUnreadBytes)
	}
	c := &Commander{}
	res := c.mcpShellRead(s, readOpts{timeoutMs: 100, quietMs: 1})
	if !strings.HasPrefix(res.text, "[output truncated: 127 unread bytes discarded]\n") || !strings.HasSuffix(res.text, "TAIL") {
		t.Fatalf("missing truncation marker or newest output, length=%d", len(res.text))
	}
	if s.buf != nil || s.dropped != 0 {
		t.Fatal("consumed data is still retained")
	}
	res = c.mcpShellRead(s, readOpts{timeoutMs: 100, quietMs: 1})
	if res.text != "" {
		t.Fatalf("incremental read repeated data: %q", res.text)
	}
}

func TestMCPConcurrentReadsConsumeEachIncrementOnce(t *testing.T) {
	s := &mcpSession{exitedCh: make(chan struct{})}
	_, _ = (&mcpBufferAdapter{s: s}).Write([]byte("only once"))
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := (&Commander{}).mcpShellRead(s, readOpts{timeoutMs: 100, quietMs: 1})
			results <- res.text
		}()
	}
	wg.Wait()
	close(results)
	combined := ""
	for out := range results {
		combined += out
	}
	if combined != "only once" {
		t.Fatalf("concurrent reads duplicated/lost output: %q", combined)
	}
}

func TestMCPNaturalExitPreservesFinalOutputAndCleansUp(t *testing.T) {
	s := &mcpSession{sid: "finalout", code: 42, exitKnown: true, exitedCh: make(chan struct{})}
	_, _ = (&mcpBufferAdapter{s: s}).Write([]byte("last output"))
	mcpSessions.Lock()
	mcpSessions.m[s.sid] = s
	mcpSessions.Unlock()
	defer mcpForgetSession(s)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); s.finish() }()
	}
	wg.Wait()
	res := (&Commander{}).mcpCall("shell_read", map[string]interface{}{"session_id": s.sid})
	if res.isErr || res.text != "session exited code=42\nlast output" {
		t.Fatalf("final read = %q, isError=%v", res.text, res.isErr)
	}
	// The structured form must expose the exit status without the caller
	// having to parse it out of the text.
	if res.structured == nil || res.structured["session_exited"] != true || res.structured["exit_code"] != 42 {
		t.Fatalf("structured exit report = %+v", res.structured)
	}
	if _, err := (&Commander{}).mcpGetSession(s.sid); err == nil {
		t.Fatal("drained exited session remains registered")
	}
}

type mcpTestStream struct {
	bytes.Buffer
	onWrite func([]byte)
}

func (s *mcpTestStream) Write(p []byte) (int, error) {
	n, err := s.Buffer.Write(p)
	if s.onWrite != nil {
		s.onWrite(p)
	}
	return n, err
}

func (s *mcpTestStream) Close() error { return nil }

func TestMCPCloseSendsRemoteTermination(t *testing.T) {
	stream := &mcpTestStream{}
	s := &mcpSession{sid: "closetest", exitedCh: make(chan struct{}), lc: &liveConn{ctl: frame.NewControl(stream)}}
	stream.onWrite = func(p []byte) {
		if bytes.Contains(p, []byte(`"close"`)) {
			s.finish() // simulate the EXIT watcher acknowledging remote teardown
		}
	}
	if _, err := (&Commander{timeout: time.Second}).mcpCloseSession(s); err != nil {
		t.Fatal(err)
	}
	env, err := frame.NewControl(stream).Recv()
	if err != nil || env.Type != proto.TClose {
		t.Fatalf("close control message = %+v, error=%v", env, err)
	}
	msg, err := proto.Decode[proto.CloseMsg](env)
	if err != nil || msg.SessionID != s.sid {
		t.Fatalf("remote termination target = %+v, error=%v", msg, err)
	}
}

func TestMCPCloseWithoutAcknowledgementTimesOut(t *testing.T) {
	s := &mcpSession{sid: "noack", exitedCh: make(chan struct{})}
	_, err := (&Commander{timeout: 5 * time.Millisecond}).mcpCloseSession(s)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unacknowledged close = %v", err)
	}
	select {
	case <-s.exitedCh:
	default:
		t.Fatal("timed-out close did not finish local cleanup")
	}
}

func TestMCPIntegerSchemasAgreeWithValidator(t *testing.T) {
	for _, tool := range mcpTools {
		name, _ := tool["name"].(string)
		schema, _ := tool["inputSchema"].(map[string]interface{})
		props, _ := schema["properties"].(map[string]interface{})
		for prop, raw := range props {
			spec, _ := raw.(map[string]interface{})
			if spec["type"] != "integer" {
				continue
			}
			if spec["maximum"] != math.MaxInt32 {
				t.Errorf("%s.%s maximum = %v; tools/list must not advertise a limit tighter than the validator accepts (%d)",
					name, prop, spec["maximum"], math.MaxInt32)
			}
		}
	}
	// Both halves of the contract: the advertised maximum is accepted, and one
	// past it is rejected instead of silently behaving like "no truncation".
	calls := []struct {
		name string
		args map[string]interface{}
	}{
		{"shell_read", map[string]interface{}{"session_id": "s", "max_output_bytes": float64(math.MaxInt32)}},
		{"exec", map[string]interface{}{"client_id": "c", "command": "x", "max_output_bytes": float64(math.MaxInt32)}},
	}
	for _, call := range calls {
		if err := mcpValidateToolArgs(call.name, call.args); err != nil {
			t.Errorf("%s rejected its advertised maximum: %v", call.name, err)
		}
		call.args["max_output_bytes"] = float64(math.MaxInt32) * 2
		if err := mcpValidateToolArgs(call.name, call.args); err == nil {
			t.Errorf("%s accepted a value above the advertised maximum", call.name)
		}
	}
}

func TestOSC133DoneScannerRequiresCompleteSequence(t *testing.T) {
	tests := []struct {
		name  string
		input string
		prior int
		done  bool
		carry string
	}{
		{name: "bare text", input: "echo 133;D\r\n"},
		{name: "bel terminated", input: "\x1b]133;D\a", done: true},
		{name: "st terminated with code", input: "before\x1b]133;D;0\x1b\\after", done: true},
		{name: "no terminator is carried", input: "x\x1b]133;D;0", carry: "\x1b]133;D;0"},
		{name: "esc at end waits for st", input: "\x1b]133;D;0\x1b", carry: "\x1b]133;D;0\x1b"},
		{name: "split prefix is carried", input: "\x1b]133", carry: "\x1b]133"},
		{name: "payload too long is not a marker", input: "\x1b]133;D" + strings.Repeat("9", 40) + "\a"},
		{name: "already reported is not repeated", input: "\x1b]133;D;0\x1b\\", prior: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, carry := scanOSC133Done([]byte(tt.input), tt.prior)
			if done != tt.done || string(carry) != tt.carry {
				t.Fatalf("done=%v carry=%q; want done=%v carry=%q", done, carry, tt.done, tt.carry)
			}
		})
	}
}

func TestMCPCommandDoneDetection(t *testing.T) {
	read := func(s *mcpSession) interface{} {
		res := (&Commander{}).mcpShellRead(s, readOpts{timeoutMs: 100, quietMs: 1})
		if res.isErr || res.structured == nil {
			t.Fatalf("read failed: %+v", res)
		}
		return res.structured["command_done"]
	}
	t.Run("command output containing 133;D is not completion", func(t *testing.T) {
		s := &mcpSession{sid: "bare", exitedCh: make(chan struct{})}
		_, _ = (&mcpBufferAdapter{s: s}).Write([]byte("echo 133;D\r\n133;D\r\n"))
		if got := read(s); got != false {
			t.Fatalf("command_done = %v; plain text must not count", got)
		}
	})
	t.Run("marker split across two reads is detected", func(t *testing.T) {
		s := &mcpSession{sid: "split", exitedCh: make(chan struct{})}
		_, _ = (&mcpBufferAdapter{s: s}).Write([]byte("partial\x1b]133;"))
		if got := read(s); got != false {
			t.Fatalf("command_done = %v before the sequence was complete", got)
		}
		_, _ = (&mcpBufferAdapter{s: s}).Write([]byte("D;0\x1b\\"))
		if got := read(s); got != true {
			t.Fatalf("command_done = %v after the marker completed", got)
		}
		if got := read(s); got != false {
			t.Fatalf("command_done = %v; a marker already reported was reported again", got)
		}
	})
}
