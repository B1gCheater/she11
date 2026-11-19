//go:build windows

package commander

import (
	"os"

	"golang.org/x/sys/windows"
)

// x/term.MakeRaw already enables VT input. Enable output processing separately
// and restore its previous mode when the interactive session ends.
func configureVT(out *os.File) (func(), error) {
	oh := windows.Handle(out.Fd())
	var om uint32
	if err := windows.GetConsoleMode(oh, &om); err != nil {
		return nil, err
	}
	// Match Unix terminal line feeds and delayed right-margin wrapping. Without
	// this, tmux repaint/scroll sequences can move to column zero or scroll early.
	mode := om | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(oh, mode); err != nil {
		return nil, err
	}
	return func() {
		_ = windows.SetConsoleMode(oh, om)
	}, nil
}
