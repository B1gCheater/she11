package commander

import "testing"

func TestInteractiveTerminalCapabilities(t *testing.T) {
	for _, tc := range []struct{ term, color, wt, wantTerm, wantColor string }{
		{"", "", "", "xterm-256color", ""},
		{"", "", "session-id", "xterm-256color", "truecolor"},
		{"screen-256color", "24bit", "", "screen-256color", "24bit"},
		{"xterm", "custom", "session-id", "xterm", "custom"},
	} {
		t.Setenv("TERM", tc.term)
		t.Setenv("COLORTERM", tc.color)
		t.Setenv("WT_SESSION", tc.wt)
		if got := interactiveTerm(); got != tc.wantTerm {
			t.Fatalf("TERM = %q, want %q", got, tc.wantTerm)
		}
		if got := interactiveColorTerm(); got != tc.wantColor {
			t.Fatalf("COLORTERM = %q, want %q", got, tc.wantColor)
		}
	}
}
