//go:build !windows

package shell

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// lookPath is a manual PATH search that avoids faccessat2 (blocked by
// Android's seccomp policy, which would SIGSYS the process). It returns an
// absolute path so downstream exec.Command never triggers Go's LookPath.
func lookPath(file string) (string, error) {
	if strings.Contains(file, "/") {
		if ok, _ := isExec(file); ok {
			return file, nil
		}
		return "", fmt.Errorf("%s: not executable", file)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, file)
		if ok, _ := isExec(p); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found in PATH", file)
}

func isExec(p string) (bool, error) {
	st, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	if st.IsDir() {
		return false, nil
	}
	return st.Mode().Perm()&0o111 != 0, nil
}

func detectAll() []string {
	var out []string
	for _, c := range []string{"fish", "zsh", "bash", "sh"} {
		if _, err := lookPath(c); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func resolve(request string) (Spec, bool) {
	login := func() []string { return []string{"-l"} }
	noArgs := func() []string { return nil }
	shExec := func(c string) []string { return []string{"-c", c} }
	withPath := func(name string, args func() []string, execf func(string) []string) (Spec, bool) {
		p, err := lookPath(name)
		if err != nil {
			return Spec{}, false
		}
		return Spec{Name: name, Path: p, Args: args, Exec: execf}, true
	}
	switch request {
	case "zsh":
		return withPath("zsh", login, func(c string) []string { return []string{"-lc", c} })
	case "bash":
		return withPath("bash", login, func(c string) []string { return []string{"-lc", c} })
	case "sh":
		return withPath("sh", noArgs, shExec)
	case "fish":
		return withPath("fish", noArgs, func(c string) []string { return []string{"-c", c} })
	case "pwsh":
		return withPath("pwsh",
			func() []string { return []string{"-NoLogo", "-NoProfile"} },
			func(c string) []string { return []string{"-NoLogo", "-NoProfile", "-Command", c} })
	case "default":
		if sh := os.Getenv("SHELL"); sh != "" {
			if s, ok := resolveByPath(sh); ok {
				return s, true
			}
		}
		for _, name := range detectAll() {
			if s, ok := resolve(name); ok {
				return s, true
			}
		}
	}
	return Spec{}, false
}

func resolveByPath(path string) (Spec, bool) {
	switch path {
	case "/bin/bash", "/usr/bin/bash", "bash", "/data/data/com.termux/files/usr/bin/bash":
		return resolve("bash")
	case "/bin/zsh", "/usr/bin/zsh", "zsh", "/data/data/com.termux/files/usr/bin/zsh":
		return resolve("zsh")
	case "/bin/fish", "/usr/bin/fish", "fish", "/data/data/com.termux/files/usr/bin/fish":
		return resolve("fish")
	case "/bin/sh", "/usr/bin/sh", "sh", "/data/data/com.termux/files/usr/bin/sh":
		return resolve("sh")
	}
	// absolute custom path: treat as bash-like if executable
	if strings.Contains(path, "/") {
		if ok, _ := isExec(path); ok {
			return Spec{Name: "sh", Path: path,
				Args: func() []string { return nil },
				Exec: func(c string) []string { return []string{"-c", c} }}, true
		}
	}
	return Spec{}, false
}
