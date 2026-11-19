package commander

import (
	"os"

	"golang.org/x/term"
)

func interactiveTerm() string {
	if v := os.Getenv("TERM"); v != "" {
		return v
	}
	return "xterm-256color"
}

func interactiveColorTerm() string {
	if v := os.Getenv("COLORTERM"); v != "" {
		return v
	}
	// Windows Terminal does not normally export COLORTERM to Windows apps.
	if os.Getenv("WT_SESSION") != "" {
		return "truecolor"
	}
	return ""
}

func prepareTerminal() (func(), error) {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	restoreVT, err := configureVT(os.Stdout)
	if err != nil {
		_ = term.Restore(int(os.Stdin.Fd()), old)
		return nil, err
	}
	return func() {
		restoreVT()
		_ = term.Restore(int(os.Stdin.Fd()), old)
	}, nil
}
