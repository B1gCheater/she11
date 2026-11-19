package client

import (
	"bytes"
	"testing"

	"she11/src/shared/proto"
)

func TestTerminalResponseOwnership(t *testing.T) {
	query := []byte("\x1b[c\x1b[>c\x1b[6n\x1b]11;?\x07")
	for _, tc := range []struct {
		name, mode, operator string
		wantAnswers          int
	}{
		{"human terminal", proto.ModePTY, "cli", 0},
		{"MCP terminal", proto.ModePTY, "mcp", 4},
		{"legacy terminal", proto.ModePTY, "", 4},
		{"legacy exec", proto.ModeExec, "cli", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := sessionResponder(proto.OpenSession{Mode: tc.mode, Operator: tc.operator})
			var replies [][]byte
			// Network/PTY reads may split any escape sequence at any byte.
			for _, b := range query {
				replies = append(replies, tr.Feed([]byte{b})...)
			}
			if len(replies) != tc.wantAnswers {
				t.Fatalf("got %q, want %d answers", replies, tc.wantAnswers)
			}
			if len(replies) > 0 && !bytes.Equal(replies[0], respDA1) {
				t.Fatalf("primary DA changed: %q", replies[0])
			}
		})
	}
}
