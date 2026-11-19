//go:build windows

// Package lock implements the single-instance mutex (compile-time name).
package lock

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32     = windows.NewLazySystemDLL("kernel32.dll")
	procCreateMx = kernel32.NewProc("CreateMutexW")
)

// ErrHeld means another instance already holds the mutex.
var ErrHeld = errors.New("another instance is already running")

type winLock struct{ h windows.Handle }

// Acquire takes the named mutex ("" = no mutex).
func Acquire(name string) (func(), error) {
	if name == "" {
		return func() {}, nil
	}
	n, _ := syscall.UTF16PtrFromString("Local\\" + name)
	h, _, _ := procCreateMx.Call(0, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		return nil, windows.GetLastError()
	}
	locked := windows.GetLastError() == windows.ERROR_ALREADY_EXISTS
	handle := windows.Handle(h)
	if locked {
		windows.CloseHandle(handle)
		return nil, ErrHeld
	}
	return func() { windows.CloseHandle(handle) }, nil
}
