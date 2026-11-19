//go:build !windows

package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFishRemoteHostIntegration(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Use this repository's asset, without installing into the user's config.
	asset := filepath.Join(cwd, "..", "..", "..", "config", "fish", "conf.d", "she11.fish")
	for _, tc := range []struct{ name, session, want string }{
		{"local", "", "normal"},
		{"remote", "probe", "yellow --bold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(fish, "--no-config", "--interactive", "--command", `
set -g fish_color_host normal
set -g fish_color_host_remote yellow --bold
source $argv[1]
emit fish_prompt
string join ' ' -- $fish_color_host
set -q SSH_TTY; and exit 9
exit 0
`, "--", asset)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "SSH_TTY=") && !strings.HasPrefix(entry, "SHE11_SESSION=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "SHE11_SESSION="+tc.session)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("fish: %v: %s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("color = %q, want %q", got, tc.want)
			}
		})
	}
}
