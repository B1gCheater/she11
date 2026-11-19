//go:build windows

package commander

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// Run in a private ConPTY, never in the user's active terminal. This tests the
// actual Windows cursor/scroll semantics rather than just checking flag values.
func TestVTConsoleRendering(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pty, err := conpty.Start(windows.ComposeCommandLine([]string{exe, "-test.run=^TestVTConsoleHelper$", "-test.v"}),
		conpty.ConPtyDimensions(40, 12),
		conpty.ConPtyEnv(append(os.Environ(), "SHE11_VT_CONSOLE_HELPER=1")))
	if err != nil {
		t.Fatal(err)
	}
	var closeOnce sync.Once
	closePTY := func() { closeOnce.Do(func() { _ = pty.Close() }) }
	defer closePTY()
	var output bytes.Buffer
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(&output, pty); close(drained) }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, waitErr := pty.Wait(ctx)
	closePTY()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("console probe did not drain")
	}
	if waitErr != nil || code != 0 {
		t.Fatalf("console probe exit=%d, err=%v, output=%q", code, waitErr, output.String())
	}
	t.Logf("private console probe: %q", output.String())
}

func TestVTConsoleHelper(t *testing.T) {
	if os.Getenv("SHE11_VT_CONSOLE_HELPER") != "1" {
		t.Skip("private ConPTY helper")
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	h := windows.Handle(out.Fd())
	var original uint32
	if err := windows.GetConsoleMode(h, &original); err != nil {
		t.Fatal(err)
	}
	defer windows.SetConsoleMode(h, original)
	base := (original | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_WRAP_AT_EOL_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) &^ windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(h, base); err != nil {
		t.Fatal(err)
	}
	position := func(s string) windows.Coord {
		t.Helper()
		if _, err := out.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		var info windows.ConsoleScreenBufferInfo
		if err := windows.GetConsoleScreenBufferInfo(h, &info); err != nil {
			t.Fatal(err)
		}
		return info.CursorPosition
	}
	// Modern ConPTY versions may already defer wrapping without this flag.
	before := position("\x1b[2;40HX")
	beforeLF := position("\x1b[2;7H\n")
	restore, err := configureVT(out)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	wrapped := position("\x1b[2;40HX")
	linefeed := position("\x1b[2;7H\n")
	t.Logf("before: wrap=%+v LF=%+v; after: wrap=%+v LF=%+v", before, beforeLF, wrapped, linefeed)
	if wrapped.X != 39 || wrapped.Y != 1 {
		t.Errorf("right margin moved/scrolled early: %+v", wrapped)
	}
	if linefeed.X != 6 || linefeed.Y != 2 {
		t.Errorf("LF should preserve column: %+v", linefeed)
	}
	// tmux uses the alternate screen and a scrolling region. A line feed at
	// the bottom margin must scroll that region without resetting its column.
	scroll := position("\x1b[?1049h\x1b[2J\x1b[2;10r\x1b[10;7H\n")
	if scroll.X != 6 || scroll.Y != 9 {
		t.Errorf("alternate-screen scroll moved cursor incorrectly: %+v", scroll)
	}
	position("\x1b[r\x1b[?1049l")
	position("\x1b[H\x1b7\x1b[7;1Hexit")
	legacyCleanup := position("\x1b[?1049l\x1b[?25h\x1b[0m")
	t.Logf("unconditional alternate-screen exit leaves cursor at %+v", legacyCleanup)
	for _, tc := range []struct {
		name, remote string
		wantY        int16
	}{
		{"ordinary shell", "exit", 7},
		{"tmux returned normally", "\x1b[?1049htmux\x1b[?1049lexit", 7},
		{"tmux disconnected", "\x1b[?1049htmux", 7},
	} {
		// Deliberately leave a stale saved cursor at the top, then place the
		// shell below existing history. Cleanup must not jump to that cursor.
		position("\x1b[H\x1b7\x1b[7;1H")
		output := &terminalOutput{w: out}
		if _, err := output.Write([]byte(tc.remote)); err != nil {
			t.Fatal(err)
		}
		output.finish()
		cursor := position("")
		if cursor.X != 0 || cursor.Y != tc.wantY {
			t.Errorf("%s cleanup jumped over history: %+v, want row %d col 0", tc.name, cursor, tc.wantY)
		}
	}
	restore()
	var restored uint32
	if err := windows.GetConsoleMode(h, &restored); err != nil {
		t.Fatal(err)
	}
	if restored != base {
		t.Errorf("output mode not restored: %x != %x", restored, base)
	}
}
