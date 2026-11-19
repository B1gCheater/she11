//go:build !windows

// Package lock implements the single-instance mutex (compile-time name).
package lock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrHeld means another instance already holds the mutex.
var ErrHeld = errors.New("another instance is already running")

// Acquire takes the named mutex via flock ("" = no mutex).
func Acquire(name string) (func(), error) {
	if name == "" {
		return func() {}, nil
	}
	path := fmt.Sprintf("/tmp/%s.lock", name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrHeld
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
