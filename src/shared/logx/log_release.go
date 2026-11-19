//go:build !debug

package logx

// Printf is a no-op in release builds (inlined away).
func Printf(format string, a ...interface{}) {}
