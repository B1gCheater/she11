// Package configpath resolves config files relative to the running binary,
// so an executable moved into PATH keeps finding the config that sits next
// to it instead of depending on the caller's working directory.
package configpath

import (
	"os"
	"path/filepath"
)

// BesideExecutable returns the path of name next to the running binary, or
// "" when the executable path cannot be resolved or the file is absent.
func BesideExecutable(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// DefaultTOML resolves name with the portable lookup order: next to the
// binary first, then the current working directory (legacy behaviour).
// Returns "" when neither location has the file.
func DefaultTOML(name string) string {
	if p := BesideExecutable(name); p != "" {
		return p
	}
	if _, err := os.Stat(name); err == nil {
		return name
	}
	return ""
}
