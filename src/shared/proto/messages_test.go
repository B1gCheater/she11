package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCapabilityFieldsRoundTrip pins the JSON names, because they are the wire
// contract between a new client, a new relay and a new commander. A rename
// here silently degrades every peer to the legacy path.
func TestCapabilityFieldsRoundTrip(t *testing.T) {
	hello := Hello{
		Role:     "client",
		ClientID: "abc12345",
		Caps:     []string{CapExecPipe, CapExecCwdEnv, CapExecSplitStderr},
	}
	raw, err := json.Marshal(hello)
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	var helloBack Hello
	if err := json.Unmarshal(raw, &helloBack); err != nil {
		t.Fatalf("unmarshal hello: %v", err)
	}
	if !reflect.DeepEqual(hello.Caps, helloBack.Caps) {
		t.Fatalf("caps round-trip = %v, want %v", helloBack.Caps, hello.Caps)
	}

	ack := HelloAck{ServerName: "ghost", Features: []string{FeatureRelayCaps, FeatureOpenCwdEnv}}
	raw, err = json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal hello_ack: %v", err)
	}
	var ackBack HelloAck
	if err := json.Unmarshal(raw, &ackBack); err != nil {
		t.Fatalf("unmarshal hello_ack: %v", err)
	}
	if !reflect.DeepEqual(ack.Features, ackBack.Features) {
		t.Fatalf("features round-trip = %v, want %v", ackBack.Features, ack.Features)
	}

	open := Open{
		ClientID: "abc12345", Shell: "cmd", Mode: ModeExecPipe,
		Cwd: `C:\tmp`, Env: []string{"FOO=bar"}, ColorTerm: "truecolor",
	}
	raw, err = json.Marshal(open)
	if err != nil {
		t.Fatalf("marshal open: %v", err)
	}
	var openBack Open
	if err := json.Unmarshal(raw, &openBack); err != nil {
		t.Fatalf("unmarshal open: %v", err)
	}
	if openBack.Cwd != open.Cwd || !reflect.DeepEqual(openBack.Env, open.Env) || openBack.Mode != ModeExecPipe {
		t.Fatalf("open round-trip = %+v, want %+v", openBack, open)
	}
	if openBack.ColorTerm != open.ColorTerm {
		t.Fatalf("open colorterm lost: %q", openBack.ColorTerm)
	}

	session := OpenSession{SessionID: "00112233", Shell: "sh", Mode: ModeExecPipe,
		Cwd: "/tmp", Env: []string{"A=b"}, ColorTerm: "24bit"}
	raw, err = json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal open_session: %v", err)
	}
	var sessionBack OpenSession
	if err := json.Unmarshal(raw, &sessionBack); err != nil {
		t.Fatalf("unmarshal open_session: %v", err)
	}
	if sessionBack.Cwd != session.Cwd || !reflect.DeepEqual(sessionBack.Env, session.Env) {
		t.Fatalf("open_session round-trip = %+v, want %+v", sessionBack, session)
	}
	if sessionBack.ColorTerm != session.ColorTerm {
		t.Fatalf("open_session colorterm lost: %q", sessionBack.ColorTerm)
	}

	info := ClientInfo{ID: "abc12345", Caps: []string{CapExecPipe}}
	raw, _ = json.Marshal(info)
	var infoBack ClientInfo
	if err := json.Unmarshal(raw, &infoBack); err != nil {
		t.Fatalf("unmarshal client_info: %v", err)
	}
	if !reflect.DeepEqual(info.Caps, infoBack.Caps) {
		t.Fatalf("client_info caps round-trip = %v, want %v", infoBack.Caps, info.Caps)
	}
}

func TestModeAndCapabilityValuesAreStable(t *testing.T) {
	// These strings appear in tool descriptions, relay features and stored
	// configs; changing one is a protocol change, not a refactor.
	cases := map[string]string{
		ModePTY:       "pty",
		ModeExec:      "exec",
		ModeExecPipe:  "exec_pipe",
		CapExecCwdEnv: "exec_cwd_env",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("constant %q changed to %q", want, got)
		}
	}
}
