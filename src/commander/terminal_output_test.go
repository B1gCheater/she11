package commander

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestTerminalOutputCleanup(t *testing.T) {
	for _, tc := range []struct{ name, output, leave string }{
		{"ordinary shell", "hello\r\nexit\r\n", ""},
		{"unterminated line", "exit", ""},
		{"tmux detached", "\x1b[?1049hfull screen\x1b[?1049lback", ""},
		{"tmux disconnected", "\x1b[?1049hfull screen", "\x1b[?1049l"},
		{"older alternate mode", "\x1b[?47hfull screen", "\x1b[?47l"},
		{"combined modes", "\x1b[?25;1047hfull screen", "\x1b[?1047l"},
		{"OSC title is not a mode", "\x1b]0;title \x1b[?1049h\x07plain", ""},
		{"DCS payload is not a mode", "\x1bPpayload \x1b[?1049h\x1b\\plain", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			out := &terminalOutput{w: &buf}
			// Deliberately split at every byte, including inside escape sequences.
			for _, b := range []byte(tc.output) {
				if _, err := out.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			if buf.String() != tc.output {
				t.Fatal("remote output was modified")
			}
			out.finish()
			want := tc.output + tc.leave + "\x1b[?25h\x1b[0m\r\n"
			if buf.String() != want {
				t.Fatalf("got %q, want %q", buf.String(), want)
			}
			out.finish()
			if buf.String() != want {
				t.Fatal("cleanup ran twice")
			}
			if _, err := out.Write([]byte("late output")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("late output accepted: %v", err)
			}
		})
	}
}
