//go:build debug

// Package logx provides build-tag-gated console logging.
// debug builds print; release builds compile empty inlined stubs.
package logx

import (
	"fmt"
	"time"
)

// Printf logs a line with a timestamp (debug builds only).
func Printf(format string, a ...interface{}) {
	fmt.Printf("[%s] "+format+"\n", append([]interface{}{time.Now().Format("15:04:05.000")}, a...)...)
}
