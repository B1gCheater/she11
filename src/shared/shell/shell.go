// Package shell detects available shells and resolves requests to concrete
// commands. Platform-specific detection lives in shell_windows.go /
// shell_other.go.
package shell

import "fmt"

// Spec describes how to launch one shell.
type Spec struct {
	Name string                    // canonical name: cmd|powershell|pwsh|bash|fish|zsh|sh|default
	Path string                    // executable path
	Args func() []string           // interactive-mode base args
	Exec func(cmd string) []string // one-shot-exec args (command included)
}

// ErrUnavailable reports a requested shell the machine cannot run.
var ErrUnavailable = fmt.Errorf("shell unavailable")

// DetectAll returns available shell names, best first.
func DetectAll() []string { return detectAll() }

// Resolve maps a request to a Spec. "default" picks the best available.
// Returns ErrUnavailable wrapped with the request name.
func Resolve(request string) (Spec, error) {
	s, ok := resolve(request)
	if !ok {
		return Spec{}, fmt.Errorf("%w: %s", ErrUnavailable, request)
	}
	return s, nil
}

// Default picks the best available shell.
func Default() (Spec, error) { return Resolve("default") }
