//go:build !windows

package identity

import (
	"fmt"
	"os"
)

// MachineSecret returns a stable per-machine secret, readable without admin.
// Cascade: /etc/machine-id -> /var/lib/dbus/machine-id -> hostname+kernel.
func MachineSecret() ([]byte, error) {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return append([]byte("linux:"), b...), nil
		}
	}
	hn, _ := os.Hostname()
	k, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return fmt.Appendf(nil, "linux:%s|%s", hn, string(k)), nil
}
