//go:build windows

package ptyx

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCloseUnblocksConPTYReaderAfterExit(t *testing.T) {
	s, err := Start(os.Getenv("ComSpec"), []string{"/Q", "/C", "echo FINAL_MARK & exit /b 42"}, os.Environ(), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Kill()
	output := make(chan string, 1)
	go func() { b, _ := io.ReadAll(s); output <- string(b) }()
	if code := s.Wait(); code != 42 {
		t.Fatalf("exit=%d, want 42", code)
	}
	s.Close()
	select {
	case out := <-output:
		if !strings.Contains(out, "FINAL_MARK") {
			t.Fatalf("missing output: %q", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ConPTY reader did not unblock after Close")
	}
	// Repeat and concurrent Close/Kill must not close reused Windows handles.
	done := make(chan struct{})
	go func() { s.Kill(); s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("repeated cleanup blocked")
	}
}

func TestKillTerminatesShellAndCollectsExit(t *testing.T) {
	s, err := Start("powershell.exe", []string{"-NoLogo", "-NoProfile", "-Command", "Start-Sleep -Seconds 60"}, os.Environ(), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Kill()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(s.pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	go io.Copy(io.Discard, s)
	done := make(chan struct{})
	go func() { s.Kill(); s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Kill/Wait blocked")
	}
	status, err := windows.WaitForSingleObject(h, 1000)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("process survived cleanup: status=%d err=%v", status, err)
	}
}
