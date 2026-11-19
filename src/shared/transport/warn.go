package transport

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var warnUDPOnce sync.Once

// warnUDPBufferOnce prints the kernel-buffer hint at most once per process,
// and only when a QUIC listener or dialer is actually created.
func warnUDPBufferOnce() {
	warnUDPOnce.Do(func() { WarnUDPBuffers(os.Stderr) })
}

// WarnUDPBuffers prints an actionable hint on Linux when the kernel caps UDP
// receive buffers below the size quic-go asks for. QUIC still works, but
// bursts can drop packets and stall otherwise idle sessions.
func WarnUDPBuffers(w io.Writer) {
	if runtime.GOOS != "linux" {
		return
	}
	b, err := os.ReadFile("/proc/sys/net/core/rmem_max")
	if err != nil {
		return
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || v <= 0 || v >= 7*1024*1024 {
		return
	}
	fmt.Fprintf(w, "she11: net.core.rmem_max=%d KiB; QUIC wants about 7 MiB. "+
		"Dropped UDP packets can stall idle sessions. Raise it with:\n"+
		"  sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000\n"+
		"  (persist in /etc/sysctl.d/99-she11.conf; or use TCP-only endpoints)\n",
		v/1024)
}
