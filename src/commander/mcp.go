// Package commander: MCP server mode (stdio).
// Tools let an AI drive remote shells with read-until-quiet semantics.
package commander

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"she11/src/shared/proto"
	"she11/src/shared/transport"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpSession holds one AI-driven shell session.
type mcpSession struct {
	sid       string
	clientID  string
	lc        *liveConn
	stm       transport.Stream
	shell     string
	mu        sync.Mutex
	readMu    sync.Mutex // concurrent HTTP reads each consume a distinct increment
	buf       []byte
	tail      []byte // incomplete escape sequence carried to the next read
	doneCarry []byte // unresolved OSC 133;D fragment seen on the raw stream
	appended  uint64 // total bytes ever appended to buf (monotonic)
	// doneScanned is the absolute raw offset already fed to the marker scanner,
	// so new bytes are scanned once no matter how often the read loop wakes up.
	doneScanned uint64
	version     uint64
	dropped     uint64
	exited      bool
	code        int
	exitKnown   bool
	exitedCh    chan struct{}
	dataDone    chan struct{}
	exitOnce    sync.Once
	closeOnce   sync.Once
}

// mcpToolResult is one tool call's outcome.
//
// text keeps working for consumers that predate structuredContent, and its
// shape is a compatibility surface: the local regression harness parses
// "session_id: x", "exit=N" and a JSON-only check_server. structured is the
// machine-readable form and must satisfy the tool's declared outputSchema
// whenever isError is false.
type mcpToolResult struct {
	text       string
	isErr      bool
	structured map[string]interface{}
}

func mcpErr(text string) mcpToolResult { return mcpToolResult{text: text, isErr: true} }

// mcpEditErr reports a session-level failure while keeping the tool's required
// structured fields present, so a caller can branch on error_code instead of
// matching message text.
func mcpEditErr(err error, base map[string]interface{}) mcpToolResult {
	if base == nil {
		base = map[string]interface{}{}
	}
	base["error_code"] = errCodeOf(err)
	base["error"] = err.Error()
	return mcpToolResult{text: err.Error(), isErr: true, structured: base}
}

// boolArg reads an optional boolean flag. Non-boolean values are rejected by
// argument validation, so this only handles the present-and-valid case.
func boolArg(args map[string]interface{}, key string) bool {
	v, _ := args[key].(bool)
	return v
}

// MCP runs the MCP server. mode: "stdio" (local, no token) | "http"
// (plain HTTP POST /mcp, optional Bearer token; intended for loopback use
// unless you tunnel it yourself — TLS is a deliberate non-goal for now).
func (c *Commander) MCP(mode, listen, token string) int {
	if len(c.cfg.Servers) == 0 {
		fmt.Fprintln(os.Stderr, "mcp: no servers configured (she11_commander.toml)")
		return 2
	}
	switch mode {
	case "stdio":
		c.mcpServe(os.Stdin, os.Stdout)
		return 0
	case "http":
		if listen == "" {
			listen = "127.0.0.1:8899"
		}
		return c.mcpServeHTTP(listen, token)
	default:
		fmt.Fprintln(os.Stderr, "mcp: unsupported mode", mode, "(stdio | http)")
		return 2
	}
}

// mcpServeHTTP serves the MCP streamable-HTTP endpoint (single-shot POST).
func (c *Commander) mcpServeHTTP(listen, token string) int {
	fmt.Fprintf(os.Stderr, "mcp http mode: listening on http://%s/mcp (token: %v, plain HTTP)\n",
		listen, token != "")
	srv := &http.Server{Addr: listen, Handler: c.mcpHTTPHandler(token), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp http:", err)
		return 1
	}
	return 0
}

func (c *Commander) mcpHTTPHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if !mcpLocalOrigin(origin) {
				mcpHTTPError(w, http.StatusForbidden, -32000, "Origin is not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if token != "" &&
			r.Header.Get("Authorization") != "Bearer "+token {
			mcpHTTPError(w, http.StatusUnauthorized, -32001, "unauthorized")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, OPTIONS")
			mcpHTTPError(w, http.StatusMethodNotAllowed, -32600, "use POST")
			return
		}
		if version := r.Header.Get("MCP-Protocol-Version"); version != "" && version != mcpProtocolVersion {
			mcpHTTPError(w, http.StatusBadRequest, -32600, "unsupported MCP-Protocol-Version")
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			mediaType, _, err := mime.ParseMediaType(ct)
			if err != nil || mediaType != "application/json" {
				mcpHTTPError(w, http.StatusUnsupportedMediaType, -32600, "Content-Type must be application/json")
				return
			}
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if err != nil {
			mcpHTTPError(w, http.StatusBadRequest, -32700, "body read error")
			return
		}
		if len(body) > 1<<20 {
			mcpHTTPError(w, http.StatusRequestEntityTooLarge, -32600, "request exceeds 1 MiB")
			return
		}
		req, invalid := mcpDecodeRequest(body)
		if invalid != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(invalid)
			return
		}
		resp := c.mcpHandle(req)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func mcpLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func mcpHTTPError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(mcpRPCError(nil, code, message))
}

func mcpRPCError(id json.RawMessage, code int, message string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func mcpDecodeRequest(raw []byte) (*rpcRequest, *rpcResponse) {
	if !json.Valid(raw) {
		return nil, mcpRPCError(nil, -32700, "parse error")
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		return nil, mcpRPCError(nil, -32600, "invalid JSON-RPC request")
	}
	if len(req.ID) > 0 {
		var id interface{}
		_ = json.Unmarshal(req.ID, &id)
		switch id.(type) {
		case string, float64, nil:
		default:
			return nil, mcpRPCError(nil, -32600, "id must be a string, number, or null")
		}
	}
	return &req, nil
}

func (c *Commander) mcpServe(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		req, resp := mcpDecodeRequest(line)
		if resp == nil {
			resp = c.mcpHandle(req)
		}
		if resp == nil { // notification, no reply
			continue
		}
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return
		}
		if f, ok := w.(interface{ Flush() }); ok {
			f.Flush()
		}
	}
	// A scanner stops silently on a read error or on a line longer than the
	// 1 MiB buffer. Report it on stderr: stdout is the JSON-RPC channel and
	// writing diagnostics there would corrupt the protocol stream.
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp stdio read error:", err)
	}
}

const (
	mcpProtocolVersion = "2025-06-18"
)

var mcpTools = []map[string]interface{}{
	{
		"name": "check_server",
		"description": "Probe she11 relay health: reachability, registered client count, uptime. " +
			"Runs an authenticated status RPC per configured relay, so 'ok' means authenticated, not merely TCP-open.",
		"inputSchema":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		"outputSchema": checkServerOutputSchema,
	},
	{
		"name": "list_client",
		"description": "List online clients, merged and deduplicated across all configured relays. " +
			"One physical device can register two transport connections (QUIC+TCP) yet appear once here; " +
			"server-side counts are connections and can exceed this roster. " +
			"Partial relay failures are reported alongside the rows instead of failing the call.",
		"inputSchema":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		"outputSchema": listClientOutputSchema,
	},
	{
		"name": "start_shell",
		"description": "Open a persistent interactive PTY session on a client and return its session_id. " +
			"State (cwd, environment, shell variables) survives across shell_write/shell_read calls in the same session. " +
			"Omitting shell_type uses the commander's configured default. " +
			"The session occupies one of the client's max-shell slots until shell_close.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"client_id": map[string]interface{}{"type": "string",
				"description": "Client id: full value, or a prefix that is unique across the whole merged fleet. An ambiguous prefix is rejected, never guessed."},
			"shell_type": map[string]interface{}{"type": "string",
				"enum":        []string{"pwsh", "powershell", "cmd", "fish", "zsh", "bash", "sh", "default"},
				"description": "Shell to launch. 'default' picks the best available on that client. Omit to use the commander-configured default."},
		}, "required": []string{"client_id"}},
		"outputSchema": startShellOutputSchema,
	},
	{
		"name": "shell_write",
		"description": "Write RAW bytes to a session's terminal input. Nothing is appended automatically: " +
			"a command is not executed until you send its terminator yourself (usually \"\\r\"). " +
			"Use this for interactive programs and individual keys (Ctrl-C is \"\\x03\"). " +
			"To run a whole command in a session, send the command text and a separate \"\\r\".",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"session_id": map[string]interface{}{"type": "string", "description": "Session id from start_shell."},
			"input":      map[string]interface{}{"type": "string", "description": "Raw input bytes. May be empty (e.g. to probe) and is not trimmed."},
		}, "required": []string{"session_id", "input"}},
		"outputSchema": shellWriteOutputSchema,
	},
	{
		"name": "shell_read",
		"description": "Read a session's new output, returning after quiet_ms of silence or timeout_ms, whichever comes first. " +
			"Incremental: each call returns only bytes produced since the previous call. " +
			"IMPORTANT: a successful read means 'output went quiet', NOT 'the command finished' — a program that pauses longer than quiet_ms looks identical to a finished one. " +
			"Keep reading until you see the completion you expect, or until the call reports session_exited. " +
			"Output is sanitized by default (terminal escape sequences stripped); pass raw_output=true for unmodified bytes.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"session_id": map[string]interface{}{"type": "string", "description": "Session id from start_shell."},
			"timeout_ms": map[string]interface{}{"type": "integer", "default": 8000, "minimum": 1, "maximum": math.MaxInt32,
				"description": "Maximum time to wait before returning whatever arrived."},
			"quiet_ms": map[string]interface{}{"type": "integer", "default": 300, "minimum": 1, "maximum": math.MaxInt32,
				"description": "Silence window that ends the read. Raise it for commands with bursty output."},
			"max_output_bytes": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": math.MaxInt32,
				"description": "Response budget for this call, separate from the 1 MiB internal buffer cap. Head and tail are kept; the omitted middle is counted. A budget above the internal cap means nothing is truncated."},
			"raw_output": map[string]interface{}{"type": "boolean", "default": false,
				"description": "true returns the exact terminal byte stream, including escape sequences."},
		}, "required": []string{"session_id"}},
		"outputSchema": shellReadOutputSchema,
	},
	{
		"name": "shell_close",
		"description": "Close a session and terminate its process tree on the client. " +
			"Waits for the remote teardown acknowledgement so the freed max-shell slot is immediately reusable; " +
			"a timeout means the acknowledgement never arrived and local cleanup proceeded anyway.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"session_id": map[string]interface{}{"type": "string", "description": "Session id from start_shell."},
		}, "required": []string{"session_id"}},
		"outputSchema": shellCloseOutputSchema,
	},
	{
		"name": "exec",
		"description": "Run a one-shot command on a client in a FRESH process and return its output and exit status. " +
			"No state carries over: cwd, environment variables and shell variables from a previous exec are gone — use start_shell for a persistent session. " +
			"The command string is handed to the remote shell, so use that platform's syntax (Windows cmd/pwsh vs POSIX). " +
			"By default the command runs without a terminal (exec_backend=pipe): stdout and stderr are captured separately and no ConPTY is created. " +
			"Pass tty=true to force the legacy terminal backend, where the two channels are merged by the OS and stdout/stderr are null. " +
			"execution_state and exit_code answer different questions and must be read separately: " +
			"'completed' with exit_code 7 is a successful call for a failing command (e.g. grep with no match), not a tool error. " +
			"'timed_out' or 'lost' means the outcome is unknown and any output captured so far is returned; " +
			"never treat that as safe to retry, because the command may already have had side effects. " +
			"timeout_ms bounds the wait for process exit AFTER the session opens; it is not a total deadline for discovery, connection and setup.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"client_id": map[string]interface{}{"type": "string",
				"description": "Client id: full value, or a prefix that is unique across the whole merged fleet. An ambiguous prefix is rejected, never guessed."},
			"command": map[string]interface{}{"type": "string",
				"description": "Command line, interpreted by the remote shell. No extra escaping or quoting is added."},
			"shell_type": map[string]interface{}{"type": "string",
				"enum":        []string{"pwsh", "powershell", "cmd", "fish", "zsh", "bash", "sh", "default"},
				"description": "Shell to run the command in. 'default' picks the best available on that client. Omit to use the commander-configured default."},
			"timeout_ms": map[string]interface{}{"type": "integer", "default": 30000, "minimum": 1, "maximum": math.MaxInt32,
				"description": "Wait for process exit after the session opens."},
			"max_output_bytes": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": math.MaxInt32,
				"description": "Response budget for this call, separate from the 1 MiB internal memory cap. Head and tail are kept; the omitted middle is counted in discarded_bytes. A budget above the internal cap means nothing is truncated."},
			"raw_output": map[string]interface{}{"type": "boolean", "default": false,
				"description": "true returns the exact byte stream, including any escape sequences the program or the terminal backend emits."},
			"cwd": map[string]interface{}{"type": "string",
				"description": "Working directory on the client. Requires the non-PTY backend, so it is rejected when tty=true or the client/relay cannot support it. The directory must exist there."},
			"env": map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"},
				"description": "Environment overrides for this command only, merged into the client's inherited environment (PATH is preserved). Requires the non-PTY backend like cwd."},
			"tty": map[string]interface{}{"type": "boolean", "default": false,
				"description": "true forces the legacy PTY backend, which merges stdout/stderr and reports both channel fields as null. Useful for programs that require a console."},
		}, "required": []string{"client_id", "command"}},
		"outputSchema": execOutputSchema,
	},
}

// Output schemas declare the machine-readable contract. The official Python
// SDK validates structuredContent against these only on success results
// (client/session.py: `not result.is_error`), so every field listed as
// required must be present on every non-error path.
//
// stdout and stderr are nullable and filled only by the pipe backend: a PTY
// merges the two streams in the OS, and reporting two fabricated channels
// would be worse than one honest null.
var (
	execOutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"execution_state": map[string]interface{}{"type": "string",
				"enum": []string{execNotStarted, execCompleted, execTimedOut, execLost},
				"description": "not_started: never submitted to a client, safe to retry. completed: ran to " +
					"completion. timed_out: submitted then killed locally. lost: submitted, connection dropped."},
			"exit_code": map[string]interface{}{"type": []string{"integer", "null"},
				"description": "Process exit status, or null when unknown (timeout, lost connection). " +
					"A non-zero code is a command result, not a tool failure."},
			"shell":     map[string]interface{}{"type": "string", "description": "Shell requested for this command."},
			"client_id": map[string]interface{}{"type": "string", "description": "Resolved client id."},
			"exec_backend": map[string]interface{}{"type": "string", "enum": []string{"", execBackendPipe, execBackendPTY},
				"description": "'pipe' runs without a terminal: real stdout/stderr separation, no ConPTY. 'pty' is the legacy " +
					"terminal-backed path, where stdout and stderr are merged by the OS and both channel fields are null. Empty when nothing was submitted."},
			"output":          map[string]interface{}{"type": "string", "description": "Captured output in arrival order. stdout and stderr are merged by the PTY backend, separate views exist only for the pipe backend."},
			"stdout":          map[string]interface{}{"type": []string{"string", "null"}, "description": "Stdout bytes for the pipe backend; null under a PTY, where the OS merged both streams."},
			"stderr":          map[string]interface{}{"type": []string{"string", "null"}, "description": "Stderr bytes for the pipe backend; null under a PTY, where the OS merged both streams."},
			"duration_ms":     map[string]interface{}{"type": "integer", "description": "Wall time of this attempt, including target resolution and session setup."},
			"truncated":       map[string]interface{}{"type": "boolean", "description": "true when output was cut to fit max_output_bytes or the internal memory cap."},
			"discarded_bytes": map[string]interface{}{"type": "integer", "description": "Bytes not present in output. Raise max_output_bytes to see more."},
			"output_raw":      map[string]interface{}{"type": "boolean", "description": "true when output is the unmodified terminal byte stream."},
			"error":           map[string]interface{}{"type": "string", "description": "Human-readable failure detail. Use error_code for decisions; do not parse this text."},
			"error_code": map[string]interface{}{"type": "string",
				"description": "Stable failure class, empty on success: not_started tools never reached the client. " +
					"target_not_found and target_ambiguous mean the target string did not resolve; relay_unreachable means no " +
					"configured relay answered; client_offline means the resolved client was not registered; shell_unavailable " +
					"means the client lacks that shell; exec_timeout/connection_lost/output_drain_timeout describe a submitted " +
					"command whose outcome is unknown."},
			"safe_to_retry": map[string]interface{}{"type": "boolean",
				"description": "true only when the command provably never reached a client. " +
					"false after timeout or lost connection, where re-running could duplicate side effects."},
		},
		"required": []string{"execution_state", "exit_code", "shell", "output", "duration_ms",
			"truncated", "discarded_bytes", "output_raw", "safe_to_retry", "error_code", "exec_backend", "stdout", "stderr"},
	}

	listClientOutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"clients": map[string]interface{}{"type": "array", "items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id":              map[string]interface{}{"type": "string"},
					"hostname":        map[string]interface{}{"type": "string"},
					"os":              map[string]interface{}{"type": "string"},
					"arch":            map[string]interface{}{"type": "string"},
					"username":        map[string]interface{}{"type": "string"},
					"via":             map[string]interface{}{"type": "string", "description": "Identity name of the relay that reported this row, not the transport. relays lists every relay that saw the client."},
					"shells":          map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Shells available on that client, best first."},
					"sightings":       map[string]interface{}{"type": "integer", "description": "Relays advertising this client id."},
					"relays":          map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					"active_sessions": map[string]interface{}{"type": "integer"},
					"max_shell":       map[string]interface{}{"type": "integer"},
					"caps": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"},
						"description": "Execution capabilities this client build advertised (exec_pipe, exec_cwd_env, exec_split_stderr). Empty for an older client; absence means 'not supported', never 'assume yes'."},
				},
				"required": []string{"id"},
			}},
			"total": map[string]interface{}{"type": "integer"},
			"error_code": map[string]interface{}{"type": "string",
				"description": "Present when the roster could not be collected: relay_unreachable, no_relay_configured. Empty or absent on success."},
			"server_errors": map[string]interface{}{"type": "array", "items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":  map[string]interface{}{"type": "string"},
					"addr":  map[string]interface{}{"type": "string"},
					"error": map[string]interface{}{"type": "string"},
				},
				"required": []string{"name", "addr", "error"},
			}, "description": "Relays that could not be queried. Their clients are absent from the roster, so an empty roster with errors is not proof that no client is online."},
		},
		"required": []string{"clients", "total"},
	}

	checkServerOutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"servers": map[string]interface{}{"type": "array", "items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":       map[string]interface{}{"type": "string"},
					"addr":       map[string]interface{}{"type": "string"},
					"status":     map[string]interface{}{"type": "string", "description": "'ok', or the failure reason."},
					"clients":    map[string]interface{}{"type": "integer", "description": "Registered client connections, which can exceed the deduplicated roster."},
					"uptime_sec": map[string]interface{}{"type": "integer"},
				},
				"required": []string{"name", "addr", "status"},
			}},
			"reachable": map[string]interface{}{"type": "integer"},
			"total":     map[string]interface{}{"type": "integer"},
			"error_code": map[string]interface{}{"type": "string",
				"description": "relay_unreachable when no configured relay answered, otherwise empty."},
		},
		"required": []string{"servers", "reachable", "total"},
	}

	startShellOutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"session_id": map[string]interface{}{"type": "string", "description": "Pass this to shell_write/shell_read/shell_close. Empty when the open failed."},
			"client_id":  map[string]interface{}{"type": "string"},
			"shell":      map[string]interface{}{"type": "string"},
			"error_code": map[string]interface{}{"type": "string", "description": "Stable failure class when the open failed (target_not_found, target_ambiguous, relay_unreachable, client_offline, shell_unavailable, session_limit, open_timeout)."},
			"error":      map[string]interface{}{"type": "string"},
		},
		"required": []string{"session_id"},
	}

	shellWriteOutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"ok":         map[string]interface{}{"type": "boolean"},
			"bytes":      map[string]interface{}{"type": "integer", "description": "Input bytes accepted by the transport."},
			"note":       map[string]interface{}{"type": "string", "description": "Reminder when the write carried no line terminator."},
			"error_code": map[string]interface{}{"type": "string", "description": "session_not_found or connection_lost when the write failed."},
			"error":      map[string]interface{}{"type": "string"},
		},
		"required": []string{"ok", "bytes"},
	}

	shellReadOutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"output":          map[string]interface{}{"type": "string", "description": "Bytes produced since the previous read."},
			"session_exited":  map[string]interface{}{"type": "boolean", "description": "true when the shell itself ended; the session is then no longer usable."},
			"exit_code":       map[string]interface{}{"type": []string{"integer", "null"}, "description": "Shell exit status once known, else null. -1 historically meant unknown."},
			"truncated":       map[string]interface{}{"type": "boolean"},
			"discarded_bytes": map[string]interface{}{"type": "integer", "description": "Bytes dropped by the response budget or the internal buffer cap."},
			"output_raw":      map[string]interface{}{"type": "boolean"},
			"command_done":    map[string]interface{}{"type": "boolean", "description": "true only when a complete OSC 133;D sequence (BEL or ST terminated) was seen on the raw terminal stream. Plain output that merely contains the text \"133;D\" does not count, and silence alone never sets this."},
			"error_code":      map[string]interface{}{"type": "string", "description": "session_not_found when the session is gone."},
			"error":           map[string]interface{}{"type": "string"},
		},
		"required": []string{"output", "session_exited", "exit_code", "truncated", "discarded_bytes", "output_raw", "command_done"},
	}

	shellCloseOutputSchema = map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"closed":     map[string]interface{}{"type": "boolean"},
			"session_id": map[string]interface{}{"type": "string"},
			"exit_code":  map[string]interface{}{"type": []string{"integer", "null"}},
			"error_code": map[string]interface{}{"type": "string", "description": "session_not_found, open_timeout or connection_lost when the close did not complete."},
			"error":      map[string]interface{}{"type": "string"},
		},
		"required": []string{"closed"},
	}
)

func (c *Commander) mcpHandle(req *rpcRequest) (resp *rpcResponse) {
	// Valid notifications never carry a JSON-RPC response, even when unknown.
	defer func() {
		if len(req.ID) == 0 {
			resp = nil
		}
	}()
	resp = &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]interface{}{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "she11_commander", "version": "0.1"},
		}
	case "notifications/initialized":
		return nil
	case "tools/list":
		resp.Result = map[string]interface{}{"tools": mcpTools}
	case "tools/call":
		var call struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "bad params"}
			return resp
		}
		if err := mcpValidateToolArgs(call.Name, call.Arguments); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: err.Error()}
			return resp
		}
		result := c.mcpCall(call.Name, call.Arguments)
		content := map[string]interface{}{"type": "text", "text": result.text}
		res := map[string]interface{}{"content": []interface{}{content}}
		if result.isErr {
			res["isError"] = true
		}
		// structuredContent is attached whenever the tool produced one,
		// including on failure. The official SDK validates it against
		// outputSchema only for non-error results, so an error payload cannot
		// trip schema checks; it still gives a caller machine-readable state
		// (execution_state, safe_to_retry) exactly when prose is least useful.
		if result.structured != nil {
			res["structuredContent"] = result.structured
		}
		resp.Result = res
	case "ping":
		resp.Result = map[string]interface{}{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("method %q not found", req.Method)}
	}
	return resp
}

var mcpSessions = struct {
	sync.Mutex
	m map[string]*mcpSession
}{m: map[string]*mcpSession{}}

// Keep unread output bounded, and allow a caller time to collect the last
// increment after natural exit. Actively running idle shells are unaffected.
const (
	mcpMaxUnreadBytes  = 1 << 20
	mcpExitedRetention = 5 * time.Minute
)

func mcpValidateToolArgs(name string, args map[string]interface{}) error {
	var required []string
	switch name {
	case "check_server", "list_client":
	case "start_shell":
		required = []string{"client_id"}
	case "shell_write":
		required = []string{"session_id", "input"}
	case "shell_read", "shell_close":
		required = []string{"session_id"}
	case "exec":
		required = []string{"client_id", "command"}
	default:
		return fmt.Errorf("unknown tool: %s", name)
	}
	for _, key := range required {
		value, ok := args[key].(string)
		if !ok || (key != "input" && strings.TrimSpace(value) == "") {
			return fmt.Errorf("%s requires string argument %q", name, key)
		}
	}
	if value, exists := args["shell_type"]; exists {
		if s, ok := value.(string); !ok || strings.TrimSpace(s) == "" {
			return fmt.Errorf("shell_type must be a nonempty string")
		}
	}
	// The bound below is also the maximum every integer inputSchema may
	// declare: advertising a tighter limit would let tools/list reject values
	// this validator accepts, so the two halves of the contract must agree.
	for _, key := range []string{"timeout_ms", "quiet_ms", "max_output_bytes"} {
		value, exists := args[key]
		if !exists {
			continue
		}
		var number float64
		switch v := value.(type) {
		case float64:
			number = v
		case int:
			number = float64(v)
		default:
			return fmt.Errorf("%s must be a positive integer", key)
		}
		if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number <= 0 || number > math.MaxInt32 {
			return fmt.Errorf("%s must be an integer between 1 and %d", key, math.MaxInt32)
		}
	}
	if value, exists := args["raw_output"]; exists {
		// JSON has a real boolean type, so anything else is a caller mistake.
		// Accepting strings would turn "yes" or "TRUE " into a silent false.
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("raw_output must be a boolean")
		}
	}
	if value, exists := args["tty"]; exists {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("tty must be a boolean")
		}
	}
	if value, exists := args["cwd"]; exists {
		s, ok := value.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return fmt.Errorf("cwd must be a nonempty string")
		}
	}
	if value, exists := args["env"]; exists {
		entries, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("env must be an object of string values")
		}
		for key, raw := range entries {
			if key == "" || strings.ContainsAny(key, "=\x00") {
				return fmt.Errorf("env key %q is not a valid variable name", key)
			}
			if _, ok := raw.(string); !ok {
				return fmt.Errorf("env[%q] must be a string", key)
			}
		}
	}
	return nil
}

func (c *Commander) mcpCall(name string, args map[string]interface{}) mcpToolResult {
	if err := mcpValidateToolArgs(name, args); err != nil {
		return mcpErr(err.Error())
	}
	str := func(k, def string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return def
	}
	num := func(k string, def int) int {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return def
	}
	switch name {
	case "check_server":
		rows, reachable := c.probeServers(c.cfg.Servers)
		servers := make([]map[string]interface{}, 0, len(rows))
		for _, row := range rows {
			servers = append(servers, map[string]interface{}{
				"name": row.name, "addr": row.addr, "status": row.status,
				"clients": row.clients, "uptime_sec": row.uptime,
			})
		}
		structured := map[string]interface{}{
			"servers": servers, "reachable": reachable, "total": len(rows)}
		if reachable == 0 {
			structured["error_code"] = errCodeRelayDown
		} else {
			structured["error_code"] = errCodeNone
		}
		// text stays parseable JSON: the regression harness feeds it straight
		// to json.loads, and this is also a readable companion for consumers
		// that ignore structuredContent.
		b, _ := json.Marshal(structured)
		return mcpToolResult{text: string(b), isErr: reachable == 0, structured: structured}
	case "list_client":
		text, structured, err := c.mcpCollectClientList()
		if err != nil {
			// Keep the roster schema satisfied on failure too: an empty roster
			// plus a code says "I could not ask", which is not the same fact as
			// "nobody is online".
			return mcpToolResult{
				text:  "list_client error: " + err.Error(),
				isErr: true,
				structured: map[string]interface{}{
					"clients": []map[string]interface{}{}, "total": 0,
					"error_code": errCodeOf(err), "error": err.Error()},
			}
		}
		return mcpToolResult{text: text, structured: structured}
	case "start_shell":
		// shell_type is forwarded as given, empty included: an omitted value
		// must reach the configured default rather than a hardcoded one.
		sess, err := c.mcpOpenSession(str("client_id", ""), str("shell_type", ""))
		if err != nil {
			return mcpToolResult{
				text:  "start_shell failed: " + err.Error(),
				isErr: true,
				structured: map[string]interface{}{
					// No session exists, so the id is present but empty: the
					// schema stays satisfied without inventing a usable id.
					"session_id": "", "client_id": str("client_id", ""),
					"shell":      str("shell_type", ""),
					"error_code": errCodeOf(err), "error": err.Error()},
			}
		}
		mcpSessions.Lock()
		mcpSessions.m[sess.sid] = sess
		mcpSessions.Unlock()
		return mcpToolResult{
			// Text format is load-bearing: callers extract the id with
			// `session_id:\s*(\S+)`.
			text: "session_id: " + sess.sid,
			structured: map[string]interface{}{
				"session_id": sess.sid, "client_id": sess.clientID, "shell": sess.shell},
		}
	case "shell_write":
		sess, err := c.mcpGetSession(str("session_id", ""))
		if err != nil {
			return mcpEditErr(err, map[string]interface{}{"ok": false, "bytes": 0})
		}
		input := args["input"]
		// fmt.Append(nil, v) is fmt.Sprint into a fresh []byte in one pass.
		data := fmt.Append(nil, input)
		return c.mcpShellWrite(sess, data)
	case "shell_read":
		sess, err := c.mcpGetSession(str("session_id", ""))
		if err != nil {
			return mcpEditErr(err, map[string]interface{}{
				"output": "", "session_exited": false, "exit_code": nil,
				"truncated": false, "discarded_bytes": 0,
				"output_raw": boolArg(args, "raw_output"), "command_done": false})
		}
		return c.mcpShellRead(sess, readOpts{
			timeoutMs: num("timeout_ms", 8000),
			quietMs:   num("quiet_ms", 300),
			maxOutput: num("max_output_bytes", 0),
			raw:       boolArg(args, "raw_output"),
		})
	case "shell_close":
		sess, err := c.mcpGetSession(str("session_id", ""))
		if err != nil {
			return mcpEditErr(err, map[string]interface{}{
				"closed": false, "session_id": str("session_id", ""), "exit_code": nil})
		}
		code, err := c.mcpCloseSession(sess)
		if err != nil {
			return mcpEditErr(err, map[string]interface{}{
				"closed": false, "session_id": sess.sid, "exit_code": nil})
		}
		structured := map[string]interface{}{"closed": true, "session_id": sess.sid}
		if code != nil {
			structured["exit_code"] = *code
		} else {
			structured["exit_code"] = nil
		}
		return mcpToolResult{text: "closed", structured: structured}
	case "exec":
		env := make([]string, 0)
		if raw, ok := args["env"].(map[string]interface{}); ok {
			for key, value := range raw {
				env = append(env, key+"="+value.(string))
			}
			// Deterministic order keeps tests and logs stable.
			sort.Strings(env)
		}
		res, err := c.execCapture(c.cfg.Servers, str("client_id", ""), str("shell_type", ""), execOptions{
			Command: str("command", ""),
			Cwd:     str("cwd", ""),
			Env:     env,
			TTY:     boolArg(args, "tty"),
			Timeout: time.Duration(num("timeout_ms", 30000)) * time.Millisecond,
		})
		structured := execStructured(res, boolArg(args, "raw_output"),
			num("max_output_bytes", 0))
		if err != nil {
			// Keep the partial output in the error text: it is the only
			// evidence of how far the command got, and dropping it is what
			// made timeouts indistinguishable from "never ran".
			msg := "exec failed: " + err.Error()
			if res != nil && res.Output != "" {
				msg += "\npartial output:\n" + structured["output"].(string)
			}
			// Failure results carry structured data too. The SDK only
			// validates outputSchema on success results, so this cannot trip
			// schema checks, and a timeout is exactly when the caller needs
			// execution_state and safe_to_retry instead of parsing prose.
			return mcpToolResult{text: msg, isErr: true, structured: structured}
		}
		text := structured["output"].(string)
		if res.ExitCode != nil {
			// `exit=N` prefix retained: the regression harness and the public
			// SDK probe both match on it, and a non-zero exit is a command
			// result rather than a tool failure.
			text = fmt.Sprintf("exit=%d\n%s", *res.ExitCode, text)
		}
		return mcpToolResult{text: text, structured: structured}
	}
	return mcpErr("unknown tool: " + name)
}

// execStructured turns one execution attempt into the declared output shape,
// applying sanitizing and the response budget. Every field the schema marks
// required is always present, including on failure paths.
func execStructured(res *execResult, raw bool, maxOutput int) map[string]interface{} {
	out := map[string]interface{}{
		"execution_state": execNotStarted,
		"exit_code":       nil,
		"shell":           "",
		"client_id":       "",
		"exec_backend":    "",
		"output":          "",
		"stdout":          nil,
		"stderr":          nil,
		"duration_ms":     0,
		"truncated":       false,
		"discarded_bytes": 0,
		"output_raw":      raw,
		"safe_to_retry":   true,
		"error_code":      errCodeNone,
	}
	if res == nil {
		out["error"] = "no execution result"
		return out
	}
	output := res.Output
	if !raw {
		output = cleanOutput(output)
	}
	discarded := res.Discarded
	truncated := discarded > 0
	if maxOutput > 0 {
		var omitted int64
		var cut bool
		output, omitted, cut = budgetOutput(output, maxOutput)
		discarded += omitted
		truncated = truncated || cut
	}
	out["execution_state"] = res.State
	if res.ExitCode != nil {
		code := *res.ExitCode
		out["exit_code"] = code
	}
	out["shell"] = res.Shell
	out["client_id"] = res.ClientID
	out["exec_backend"] = res.Backend
	out["output"] = output
	// The channel views are only set by the pipe backend. Under a PTY they
	// stay null: two fabricated streams would be worse than one honest merge.
	if res.Stdout != nil {
		s := *res.Stdout
		if !raw {
			s = cleanOutput(s)
		}
		out["stdout"] = s
	}
	if res.Stderr != nil {
		s := *res.Stderr
		if !raw {
			s = cleanOutput(s)
		}
		out["stderr"] = s
	}
	out["duration_ms"] = res.DurationMs
	out["truncated"] = truncated
	out["discarded_bytes"] = discarded
	out["safe_to_retry"] = res.SafeToRetry
	if res.Error != "" {
		out["error"] = res.Error
	}
	out["error_code"] = res.ErrorCode
	return out
}

func (c *Commander) mcpOpenSession(clientID, shellType string) (*mcpSession, error) {
	lc, realID, err := c.pickTarget(c.cfg.Servers, clientID, c.cfg.General.Transport)
	if err != nil {
		return nil, err
	}
	// An omitted shell_type must reach the commander-configured default, not a
	// hardcoded "default". Previously MCP passed the literal "default" and the
	// config value was silently bypassed on both start_shell and exec.
	if shellType == "" {
		shellType = c.cfg.General.ShellType
	}
	o := proto.Open{ClientID: realID, Shell: shellType, Mode: "pty",
		Cols: 200, Rows: 50, Operator: "mcp"}
	stm, sid, err := c.openSession(lc, o)
	if err != nil {
		lc.conn.CloseWithError(0, "x")
		return nil, err
	}
	sess := &mcpSession{sid: sid, clientID: realID, lc: lc, stm: stm, shell: shellType,
		code: -1, exitedCh: make(chan struct{}), dataDone: make(chan struct{})}
	go func() {
		_, _ = io.Copy(&mcpBufferAdapter{s: sess}, stm)
		close(sess.dataDone)
		// EXIT travels on a different stream, so EOF must not overwrite its
		// code or discard its final bytes. Also bound a missing EXIT frame.
		timer := time.NewTimer(c.timeout)
		defer timer.Stop()
		select {
		case <-sess.exitedCh:
		case <-timer.C:
			sess.finish()
		}
	}()
	// EXIT watcher on control stream
	go func() {
		for {
			env, err := lc.ctl.Recv()
			if err != nil {
				sess.awaitDrain(c.timeout)
				sess.finish()
				return
			}
			if env.Type == proto.TExit {
				m, err := proto.Decode[proto.ExitMsg](env)
				if err != nil || m.SessionID != sess.sid {
					continue
				}
				sess.mu.Lock()
				sess.exitKnown = true
				sess.code = m.Code
				sess.mu.Unlock()
				// Only advertise exit once the data reader has drained the stream.
				sess.awaitDrain(c.timeout)
				sess.finish()
				return
			}
		}
	}()
	return sess, nil
}

// mcpBufferAdapter appends to the session buffer.
type mcpBufferAdapter struct{ s *mcpSession }

func (a *mcpBufferAdapter) Write(p []byte) (int, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if a.s.exited {
		return len(p), nil
	}
	if len(p) >= mcpMaxUnreadBytes {
		a.s.dropped += uint64(len(a.s.buf) + len(p) - mcpMaxUnreadBytes)
		a.s.buf = append(a.s.buf[:0], p[len(p)-mcpMaxUnreadBytes:]...)
	} else {
		if overflow := len(a.s.buf) + len(p) - mcpMaxUnreadBytes; overflow > 0 {
			a.s.dropped += uint64(overflow)
			a.s.buf = a.s.buf[:copy(a.s.buf, a.s.buf[overflow:])]
		}
		a.s.buf = append(a.s.buf, p...)
	}
	if len(p) > 0 {
		a.s.appended += uint64(len(p))
		a.s.version++
	}
	return len(p), nil
}

func (c *Commander) mcpGetSession(sid string) (*mcpSession, error) {
	mcpSessions.Lock()
	defer mcpSessions.Unlock()
	s := mcpSessions.m[sid]
	if s == nil {
		return nil, codedErrorf(errCodeSessionNotFound, "no session %q", sid)
	}
	return s, nil
}

func (c *Commander) mcpShellWrite(s *mcpSession, data []byte) mcpToolResult {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return mcpEditErr(codedErrorf(errCodeSessionNotFound, "session already exited"),
			map[string]interface{}{"ok": false, "bytes": 0})
	}
	if _, err := s.stm.Write(data); err != nil {
		return mcpEditErr(codedErrorf(errCodeLost, "write failed: %v", err),
			map[string]interface{}{"ok": false, "bytes": 0})
	}
	structured := map[string]interface{}{"ok": true, "bytes": len(data)}
	text := "ok"
	// Input is deliberately passed through unmodified, so nothing executes
	// until the caller supplies a terminator. Saying so at write time is what
	// stops an AI from reading the echo back and concluding the command ran.
	if len(data) > 0 && !bytes.ContainsAny(data, "\r\n") {
		note := "input accepted as raw bytes; no line terminator included, so nothing will execute until one is sent"
		structured["note"] = note
		text = "ok (no line terminator sent: command will not execute yet)"
	}
	return mcpToolResult{text: text, structured: structured}
}

// readOpts are one shell_read call's parameters.
type readOpts struct {
	timeoutMs int
	quietMs   int
	maxOutput int
	raw       bool
}

// readOutcome is what one incremental read observed.
type readOutcome struct {
	output     string
	exited     bool
	code       *int
	dropped    uint64
	cmdDone    bool
	sanitized  bool
	incomplete []byte // escape sequence split across reads, carried to next time
}

func (c *Commander) mcpShellRead(s *mcpSession, opts readOpts) mcpToolResult {
	oc := c.mcpShellReadRaw(s, opts)
	structured := map[string]interface{}{
		"output":          oc.output,
		"session_exited":  oc.exited,
		"exit_code":       nil,
		"truncated":       oc.dropped > 0,
		"discarded_bytes": int64(oc.dropped),
		"output_raw":      opts.raw,
		"command_done":    oc.cmdDone,
	}
	if oc.code != nil {
		structured["exit_code"] = *oc.code
	}
	text := oc.output
	if oc.dropped > 0 {
		text = fmt.Sprintf("[output truncated: %d unread bytes discarded]\n", oc.dropped) + text
	}
	if oc.exited {
		code := -1
		if oc.code != nil {
			code = *oc.code
		}
		text = fmt.Sprintf("session exited code=%d\n%s", code, text)
		mcpForgetSession(s)
	}
	return mcpToolResult{text: text, structured: structured}
}

const (
	osc133DonePrefix = "\x1b]133;D"
	osc133MaxPayload = 32 // ";<exit code>" plus slack; longer payloads are not markers
	osc133MaxCarry   = len(osc133DonePrefix) + osc133MaxPayload + 2
)

// scanOSC133Done searches raw terminal bytes for a complete OSC 133;D sequence,
// the completion marker emitted by shells with shell integration. The payload
// must end with BEL or ST, so command output that merely contains the text
// "133;D" cannot pass for completion.
//
// The first prior bytes are a carry from an earlier scan that already went
// through this function; a match ending inside them was already reported, so
// only a match ending after them sets done. carry is the trailing fragment that
// may complete into a marker once more bytes arrive, which is what keeps a
// sequence split across two reads detectable.
func scanOSC133Done(b []byte, prior int) (done bool, carry []byte) {
	prefix := []byte(osc133DonePrefix)
	for i := 0; i < len(b); {
		rel := bytes.Index(b[i:], prefix)
		if rel < 0 {
			if n := partialPrefixLen(b[i:], prefix); n > 0 {
				carry = append([]byte(nil), b[len(b)-n:]...)
			}
			return done, carry
		}
		start := i + rel
		end, complete := osc133PayloadEnd(b, start+len(prefix))
		if !complete {
			// Keep the unfinished candidate bounded: a marker split across reads
			// is short, and an endless "payload" is not a marker at all.
			if len(b)-start <= osc133MaxCarry {
				return done, append([]byte(nil), b[start:]...)
			}
			return done, nil
		}
		if end < 0 {
			i = start + len(prefix) // not a marker after all; keep scanning
			continue
		}
		if end > prior {
			done = true
		}
		i = end
	}
	return done, nil
}

// osc133PayloadEnd returns the offset just past the OSC terminator that starts
// at p. complete=false means the bytes end before any terminator, so the caller
// must carry them; end<0 with complete=true means the bytes at p cannot be a
// marker: an ESC that starts no ST, or a payload longer than any real one.
func osc133PayloadEnd(b []byte, p int) (end int, complete bool) {
	for q := p; q < len(b); q++ {
		switch {
		case b[q] == '\a':
			return q + 1, true
		case b[q] == 0x1b:
			if q+1 == len(b) {
				return 0, false // ST may still be arriving
			}
			if b[q+1] == '\\' {
				return q + 2, true
			}
			return -1, true
		case q-p >= osc133MaxPayload:
			return -1, true
		}
	}
	return 0, false
}

// partialPrefixLen is the length of the longest suffix of b that is a proper
// prefix of prefix. It is what lets a marker split mid-prefix survive the read
// boundary.
func partialPrefixLen(b, prefix []byte) int {
	max := len(prefix) - 1
	if len(b) < max {
		max = len(b)
	}
	for n := max; n > 0; n-- {
		if bytes.Equal(b[len(b)-n:], prefix[:n]) {
			return n
		}
	}
	return 0
}

// mcpShellReadRaw reads one increment, applying sanitizing unless raw output
// was requested. Held under readMu so concurrent readers still consume
// distinct increments.
func (c *Commander) mcpShellReadRaw(s *mcpSession, opts readOpts) readOutcome {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	quietMs, timeoutMs := opts.quietMs, opts.timeoutMs
	if quietMs <= 0 {
		quietMs = 300
	}
	if timeoutMs <= 0 {
		timeoutMs = 8000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	lastData := time.Now()
	cmdDoneSeen := false
	var version uint64
	first := true
	// scanNew feeds the raw bytes appended since the previous scan to the marker
	// scanner, carrying an unresolved fragment in the session so a marker split
	// across two reads is still detected. caller must hold s.mu.
	scanNew := func() {
		if cmdDoneSeen {
			return
		}
		base := s.appended - uint64(len(s.buf)) // absolute index of buf[0]
		if s.doneScanned < base {
			// The unread cap dropped bytes the scanner never saw, so a carried
			// fragment no longer continues into what is still retained.
			s.doneScanned, s.doneCarry = base, nil
		}
		if s.doneScanned >= s.appended {
			return
		}
		prior := len(s.doneCarry)
		chunk := make([]byte, 0, prior+int(s.appended-s.doneScanned))
		chunk = append(chunk, s.doneCarry...)
		chunk = append(chunk, s.buf[s.doneScanned-base:]...)
		s.doneScanned = s.appended
		seen, carry := scanOSC133Done(chunk, prior)
		if seen {
			cmdDoneSeen, s.doneCarry = true, nil
			return
		}
		s.doneCarry = append(s.doneCarry[:0], carry...)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		hasNew := s.version != version || (first && len(s.buf) > 0)
		version = s.version
		exited := s.exited
		// OSC 133;D = the shell's "command finished" marker (bash/fish/zsh
		// with shell integration): once seen, only a short silence is needed.
		// Only a complete BEL/ST-terminated sequence counts, matched on raw
		// bytes before any sanitizing.
		if hasNew {
			scanNew()
		}
		s.mu.Unlock()
		first = false
		if hasNew {
			lastData = time.Now()
		}
		silenceNeed := quietMs
		if cmdDoneSeen {
			silenceNeed = 120
		}
		if exited || time.Since(lastData) >= time.Duration(silenceNeed)*time.Millisecond {
			break // quiet window reached (read-until-quiet)
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-s.exitedCh:
		case <-ticker.C:
		}
	}
	s.mu.Lock()
	scanNew()                       // bytes that landed after the last scan must not skip detection
	raw := append(s.tail, s.buf...) // tail: sequence split across previous reads
	s.buf = nil                     // release consumed history rather than retaining its allocation
	dropped := s.dropped
	s.dropped = 0
	exited, code, exitKnown := s.exited, s.code, s.exitKnown
	s.mu.Unlock()

	out := readOutcome{output: string(raw), exited: exited, dropped: dropped, cmdDone: cmdDoneSeen}
	if exitKnown {
		out.code = &code
	}
	if opts.raw {
		s.mu.Lock()
		s.tail = nil
		s.mu.Unlock()
		return out
	}
	// Sanitizing can split an escape sequence across two reads; the incomplete
	// tail must not reach the caller as garbage, nor be silently dropped.
	res := sanitizeTerminal(raw)
	out.output = res.text
	out.sanitized = true
	if len(res.tail) > 0 {
		if out.exited {
			// No further read will ever come, so flush instead of lose it.
			out.output += cleanOutput(string(res.tail))
		} else {
			s.mu.Lock()
			s.tail = append(s.tail[:0], res.tail...)
			s.mu.Unlock()
		}
	}
	if opts.maxOutput > 0 {
		var omitted int64
		var cut bool
		out.output, omitted, cut = budgetOutput(out.output, opts.maxOutput)
		out.dropped += uint64(omitted)
		_ = cut
	}
	return out
}

func (c *Commander) mcpCloseSession(s *mcpSession) (*int, error) {
	defer mcpForgetSession(s)
	defer func() {
		s.closeTransport()
		s.finish()
		s.mu.Lock()
		s.buf = nil
		s.tail = nil
		s.doneCarry = nil
		s.dropped = 0
		s.mu.Unlock()
	}()
	timeout := c.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// A QUIC stream Close only closes its write side. The control message is
	// what asks the client to terminate its process and release its quota.
	if s.lc != nil && s.lc.ctl != nil {
		if s.lc.conn != nil {
			stop := boundControl(s.lc, timeout)
			defer stop()
		}
		if err := s.lc.ctl.Send(proto.TClose, proto.CloseMsg{SessionID: s.sid}); err != nil {
			return nil, codedErrorf(errCodeLost, "send close: %v", err)
		}
	}
	// EXIT acknowledges remote teardown. Keeping the connection until it
	// arrives lets a following open reuse the released quota immediately.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.exitedCh:
		s.mu.Lock()
		known := s.exitKnown
		code := s.code
		s.mu.Unlock()
		if !known {
			return nil, nil
		}
		return &code, nil
	case <-timer.C:
		return nil, codedErrorf(errCodeOpenTimeout, "timed out waiting for remote exit; connection closed")
	}
}

func (s *mcpSession) closeTransport() {
	s.closeOnce.Do(func() {
		if s.stm != nil {
			_ = s.stm.Close()
		}
		if s.lc != nil && s.lc.conn != nil {
			_ = s.lc.conn.CloseWithError(0, "closed")
		}
	})
}

func (s *mcpSession) awaitDrain(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.dataDone:
	case <-timer.C:
		s.closeTransport()
		<-s.dataDone
	}
}

func (s *mcpSession) finish() {
	s.exitOnce.Do(func() {
		s.mu.Lock()
		s.exited = true
		if !s.exitKnown {
			s.code = -1
		}
		s.mu.Unlock()
		close(s.exitedCh)
		s.closeTransport()
		time.AfterFunc(mcpExitedRetention, func() { mcpForgetSession(s) })
	})
}

func mcpForgetSession(s *mcpSession) {
	mcpSessions.Lock()
	if mcpSessions.m[s.sid] == s {
		delete(mcpSessions.m, s.sid)
	}
	mcpSessions.Unlock()
}

// mcpCollectClientList returns the roster as both a text table and structured
// rows. The text keeps its line-oriented shape because callers locate a client
// by matching a line's leading id; the structured rows carry the detail text
// cannot express (per-relay sightings, session capacity).
func (c *Commander) mcpCollectClientList() (string, map[string]interface{}, error) {
	// Snapshots are used directly rather than via queryClients so that
	// per-relay sightings survive the merge. Same three steps, same result.
	snapshots := c.clientSnapshots(c.cfg.Servers, c.cfg.General.Transport)
	defer closeSnapshots(snapshots, nil)
	clients, failures, err := mergeSnapshots(snapshots)
	if err != nil {
		return "", nil, err
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].ID < clients[j].ID })

	// One device can register through several relays; the deduplicated roster
	// hides that, which is why a relay's client count can exceed the roster.
	sightings := map[string][]string{}
	for _, snapshot := range snapshots {
		if snapshot.err != nil {
			continue
		}
		for _, cl := range snapshot.clients {
			sightings[cl.ID] = append(sightings[cl.ID], snapshot.server.Name)
		}
	}

	rows := make([]string, 0, len(clients)+len(failures)+1)
	out := make([]map[string]interface{}, 0, len(clients))
	for _, cl := range clients {
		shells := cl.Shells
		if shells == nil {
			shells = []string{}
		}
		relays := sightings[cl.ID]
		if relays == nil {
			relays = []string{}
		}
		caps := cl.Caps
		if caps == nil {
			caps = []string{}
		}
		rows = append(rows, fmt.Sprintf("%s %s %s/%s %s via=%s shells=%s",
			cl.ID, cl.Hostname, cl.OS, cl.Arch, cl.Username, cl.Via, strings.Join(cl.Shells, ",")))
		out = append(out, map[string]interface{}{
			"id": cl.ID, "hostname": cl.Hostname, "os": cl.OS, "arch": cl.Arch,
			"username": cl.Username, "via": cl.Via, "shells": shells,
			"sightings": len(relays), "relays": relays,
			"active_sessions": cl.ActiveSessions, "max_shell": cl.MaxShell,
			"caps": caps,
		})
	}
	if len(clients) == 0 {
		rows = append(rows, "(no clients)")
	}
	errs := make([]map[string]interface{}, 0, len(failures))
	for _, failure := range failures {
		rows = append(rows, fmt.Sprintf("warning: server %s (%s): %s", failure.Name, failure.Addr, failure.Error))
		errs = append(errs, map[string]interface{}{
			"name": failure.Name, "addr": failure.Addr, "error": failure.Error})
	}
	structured := map[string]interface{}{"clients": out, "total": len(clients)}
	if len(errs) > 0 {
		structured["server_errors"] = errs
	}
	return strings.Join(rows, "\n"), structured, nil
}
