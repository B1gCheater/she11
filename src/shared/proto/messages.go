// Package proto defines the control-plane messages exchanged over the
// control stream. Frames are JSON (low-frequency control plane; the data
// plane stays raw bytes per session stream).
package proto

import "encoding/json"

// Control message types.
const (
	THello         = "hello"
	THelloAck      = "hello_ack"
	TAuth          = "auth"
	TAuthResult    = "auth_result"
	THeartbeat     = "heartbeat"
	THeartbeatAck  = "heartbeat_ack"
	TListClients   = "list_clients"
	TClientList    = "client_list"
	TServerInfoReq = "server_info_req"
	TServerInfo    = "server_info"
	TOpen          = "open"
	TOpenResult    = "open_result"
	TOpenSession   = "open_session"
	TSessionAccept = "session_accept"
	TSessionReject = "session_reject"
	TResize        = "resize"
	TExit          = "exit"
	TClose         = "close"
	TGoingAway     = "going_away"
)

// Error codes (human-readable Reason must always accompany non-zero codes).
const (
	CodeOK               = 0
	CodeClientOffline    = 1
	CodeClientDenied     = 2
	CodeAuthzDenied      = 3
	CodeShellUnavailable = 4
	CodeSessionLimit     = 5
	CodeServerBusy       = 6
	CodeTimeout          = 7
	CodeProtoMismatch    = 8
	CodeNotEnrolled      = 9
	CodeBadMagic         = 10
	CodeBadPassword      = 11
	CodeNoShell          = 12
	CodeExecCwd          = 13
	CodeExecEnv          = 14
)

// Session modes. "pty" is an interactive terminal, "exec" is the legacy
// one-shot path that still uses a PTY, and "exec_pipe" is the non-PTY
// one-shot path with tagged stdout/stderr frames. A client must reject any
// mode it does not know instead of treating it as interactive.
const (
	ModePTY      = "pty"
	ModeExec     = "exec"
	ModeExecPipe = "exec_pipe"
)

// Capability and feature strings. These are additive: an absent string means
// "not supported", so an old peer simply falls back to the legacy path.
const (
	// Client capabilities, advertised in Hello.Caps.
	CapExecPipe        = "exec_pipe"
	CapExecCwdEnv      = "exec_cwd_env"
	CapExecSplitStderr = "exec_split_stderr"

	// Relay features, advertised in HelloAck.Features. A commander must not
	// send fields a relay cannot forward: an old relay decodes Open into a
	// fixed struct and silently drops unknown JSON fields.
	FeatureRelayCaps  = "relay_caps"
	FeatureOpenCwdEnv = "relay_open_cwd_env"
)

// Envelope wraps every control frame.
type Envelope struct {
	Type string          `json:"type"`
	Seq  uint64          `json:"seq"`
	Msg  json.RawMessage `json:"msg,omitempty"`
}

// NewEnvelope builds an envelope with a marshalled payload.
func NewEnvelope(t string, seq uint64, msg interface{}) (*Envelope, error) {
	var raw json.RawMessage
	if msg != nil {
		b, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return &Envelope{Type: t, Seq: seq, Msg: raw}, nil
}

// Decode unmarshals the envelope payload into T.
func Decode[T any](e *Envelope) (T, error) {
	var v T
	err := json.Unmarshal(e.Msg, &v)
	return v, err
}

// Hello is sent by both roles right after opening the control stream.
type Hello struct {
	Role        string   `json:"role"` // "client" | "commander"
	Magic       string   `json:"magic,omitempty"`
	ClientID    string   `json:"client_id,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	OS          string   `json:"os,omitempty"`
	Arch        string   `json:"arch,omitempty"`
	Username    string   `json:"username,omitempty"`
	Shells      []string `json:"shells,omitempty"`
	MaxShell    int      `json:"max_shell,omitempty"`
	Caps        []string `json:"caps,omitempty"`
}

// HelloAck is sent by the server once the peer identity is validated.
type HelloAck struct {
	ServerName string   `json:"server_name"`
	Features   []string `json:"features,omitempty"`
}

// Auth carries the commander password (inside TLS).
type Auth struct {
	Password string `json:"password"`
}

// AuthResult answers an Auth.
type AuthResult struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Heartbeat proves the peer process is alive and healthy.
type Heartbeat struct {
	ActiveSessions int `json:"active_sessions"`
}

// HeartbeatAck answers a heartbeat.
type HeartbeatAck struct{}

// ListClients requests the online client roster.
type ListClients struct{}

// ClientInfo is one row of the roster.
type ClientInfo struct {
	ID             string   `json:"id"`
	Hostname       string   `json:"hostname,omitempty"`
	OS             string   `json:"os,omitempty"`
	Arch           string   `json:"arch,omitempty"`
	Username       string   `json:"username,omitempty"`
	Shells         []string `json:"shells,omitempty"`
	ObservedAddr   string   `json:"observed_addr,omitempty"`
	Via            string   `json:"via,omitempty"`
	ConnectedAt    int64    `json:"connected_at,omitempty"`
	LastSeen       int64    `json:"last_seen,omitempty"`
	ActiveSessions int      `json:"active_sessions,omitempty"`
	MaxShell       int      `json:"max_shell,omitempty"`
	Caps           []string `json:"caps,omitempty"`
}

// ClientList is the server's answer.
type ClientList struct {
	Clients    []ClientInfo `json:"clients"`
	ServerName string       `json:"server_name,omitempty"`
}

// ServerInfoReq requests server status.
type ServerInfoReq struct{}

// ServerInfo answers it.
type ServerInfo struct {
	Name         string `json:"name"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Version      string `json:"version,omitempty"`
	UptimeSec    int64  `json:"uptime_sec"`
	ClientCount  int    `json:"client_count"`
	SessionCount int    `json:"session_count"`
	ObservedAddr string `json:"observed_addr,omitempty"`
	PublicAddr   string `json:"public_addr,omitempty"`
}

// Open asks the server to open a session to a client (commander -> server).
type Open struct {
	ClientID  string   `json:"client_id"`
	Shell     string   `json:"shell"` // cmd|powershell|pwsh|bash|fish|zsh|sh|default
	Mode      string   `json:"mode"`  // pty | exec | exec_pipe
	Term      string   `json:"term,omitempty"`
	ColorTerm string   `json:"colorterm,omitempty"`
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	Command   string   `json:"command,omitempty"`
	Operator  string   `json:"operator,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Env       []string `json:"env,omitempty"`
}

// OpenResult reports the session setup outcome.
type OpenResult struct {
	OK        bool   `json:"ok"`
	Code      int    `json:"code"`
	Reason    string `json:"reason,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// OpenSession is the reverse-flow request (server -> client control stream).
type OpenSession struct {
	SessionID string   `json:"session_id"`
	Shell     string   `json:"shell"`
	Mode      string   `json:"mode"`
	Term      string   `json:"term,omitempty"`
	ColorTerm string   `json:"colorterm,omitempty"`
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	Command   string   `json:"command,omitempty"`
	Operator  string   `json:"operator,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Env       []string `json:"env,omitempty"`
}

// SessionAccept confirms a spawned shell (client -> server).
type SessionAccept struct {
	SessionID string `json:"session_id"`
}

// SessionReject refuses a spawn.
type SessionReject struct {
	SessionID string `json:"session_id"`
	Code      int    `json:"code"`
	Reason    string `json:"reason"`
}

// Resize updates the PTY size.
type Resize struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// ExitMsg reports the session end.
type ExitMsg struct {
	SessionID string `json:"session_id"`
	Code      int    `json:"code"`
	Signal    int    `json:"signal,omitempty"`
}

// CloseMsg asks to terminate a session.
type CloseMsg struct {
	SessionID string `json:"session_id"`
}

// GoingAway announces graceful shutdown.
type GoingAway struct {
	Reason string `json:"reason,omitempty"`
}
