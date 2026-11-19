//go:build windows

package shell

import (
	"os"
	"os/exec"
	"path/filepath"
)

func detectAll() []string {
	var out []string
	if _, err := exec.LookPath("pwsh.exe"); err == nil {
		out = append(out, "pwsh")
	}
	if _, err := exec.LookPath("powershell.exe"); err == nil {
		out = append(out, "powershell")
	}
	out = append(out, "cmd") // always present
	if bashPath() != "" {
		out = append(out, "bash")
	}
	return out
}

func bashPath() string {
	cands := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "bin", "bash.exe"),
	}
	if p, err := exec.LookPath("bash.exe"); err == nil {
		cands = append([]string{p}, cands...)
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func resolve(request string) (Spec, bool) {
	cmdPath := func() string {
		if p := os.Getenv("ComSpec"); p != "" {
			return p
		}
		return "cmd.exe"
	}
	psArgs := []string{"-NoLogo", "-NoProfile"}
	switch request {
	case "cmd":
		return Spec{Name: "cmd", Path: cmdPath(),
			Args: func() []string { return []string{"/Q"} },
			Exec: func(c string) []string { return []string{"/Q", "/C", c} }}, true
	case "powershell":
		return Spec{Name: "powershell", Path: "powershell.exe",
			Args: func() []string { return psArgs },
			Exec: func(c string) []string { return append(append([]string{}, psArgs...), "-Command", c) }}, true
	case "pwsh":
		return Spec{Name: "pwsh", Path: "pwsh.exe",
			Args: func() []string { return psArgs },
			Exec: func(c string) []string { return append(append([]string{}, psArgs...), "-Command", c) }}, true
	case "bash":
		if p := bashPath(); p != "" {
			return Spec{Name: "bash", Path: p,
				Args: func() []string { return []string{"-l", "-i"} },
				Exec: func(c string) []string { return []string{"-lc", c} }}, true
		}
	case "default":
		for _, name := range detectAll() {
			if name == "default" {
				continue
			}
			if s, ok := resolve(name); ok {
				return s, true
			}
		}
	}
	return Spec{}, false
}
