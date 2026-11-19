//go:build windows

package pipex

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startCMD runs one cmd.exe command through pipex.
func startCMD(t *testing.T, command string, env []string, cwd string) *Proc {
	t.Helper()
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	if env == nil {
		env = os.Environ()
	}
	p, err := Start(comspec, []string{"/Q", "/C", command}, env, cwd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return p
}

// drainBoth reads stdout and stderr to EOF before Wait, as the client does.
func drainBoth(t *testing.T, p *Proc) (string, string) {
	t.Helper()
	type res struct {
		which string
		data  []byte
	}
	ch := make(chan res, 2)
	go func() { b, _ := io.ReadAll(p.Stdout); ch <- res{"stdout", b} }()
	go func() { b, _ := io.ReadAll(p.Stderr); ch <- res{"stderr", b} }()
	out := map[string]string{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-ch:
			out[r.which] = string(r.data)
		case <-time.After(20 * time.Second):
			p.Kill()
			t.Fatalf("pipes did not reach EOF; reads hang past a closed stdin")
		}
	}
	return out["stdout"], out["stderr"]
}

func TestStreamsSeparateAndExitCode(t *testing.T) {
	p := startCMD(t, `echo OUT_MARK & echo ERR_MARK 1>&2 & exit 42`, nil, "")
	stdout, stderr := drainBoth(t, p)
	code := p.Wait()
	p.Close()
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
	if !strings.Contains(stdout, "OUT_MARK") || strings.Contains(stdout, "ERR_MARK") {
		t.Fatalf("stdout = %q, want only OUT_MARK", stdout)
	}
	if !strings.Contains(stderr, "ERR_MARK") || strings.Contains(stderr, "OUT_MARK") {
		t.Fatalf("stderr = %q, want only ERR_MARK", stderr)
	}
}

func TestStdinIsClosedAtSpawn(t *testing.T) {
	// set /p blocks while stdin is open and falls through on EOF.
	p := startCMD(t, `set /p X= & echo GOT_EOF`, nil, "")
	stdout, _ := drainBoth(t, p)
	if code := p.Wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	p.Close()
	if !strings.Contains(stdout, "GOT_EOF") {
		t.Fatalf("stdout = %q: the child did not see EOF on stdin", stdout)
	}
}

func TestCwdAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	env := append(os.Environ(), "SHE11_PIPEX_TEST=value-42")
	p := startCMD(t, `cd & echo ENV=%SHE11_PIPEX_TEST%`, env, dir)
	stdout, _ := drainBoth(t, p)
	p.Wait()
	p.Close()
	wantDir := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		wantDir = resolved
	}
	if !strings.Contains(stdout, wantDir) && !strings.Contains(stdout, strings.ToUpper(wantDir)) {
		t.Fatalf("stdout = %q, want cwd %q", stdout, wantDir)
	}
	if !strings.Contains(stdout, "ENV=value-42") {
		t.Fatalf("stdout = %q, want the env override", stdout)
	}
}

func TestKillTerminatesRunningProcess(t *testing.T) {
	p := startCMD(t, `ping -n 30 127.0.0.1 >nul`, nil, "")
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	p.Kill()
	done := make(chan int, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("kill took %s", elapsed)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("process survived Kill")
	}
	p.Close()
}

// TestRepeatedStartsDoNotLeakHandles is the regression for why the non-PTY
// backend exists: 20 ConPTY creations grew this process's handle count by
// about one per exec, while pipe starts reach a steady state.
//
// The measurement compares two consecutive batches of 20: the first batch can
// legitimately create runtime threads whose handles persist, so only the
// second batch's delta proves there is no per-exec leak.
func TestRepeatedStartsDoNotLeakHandles(t *testing.T) {
	batch := func() {
		for i := 0; i < 20; i++ {
			p := startCMD(t, `exit 0`, nil, "")
			drainBoth(t, p)
			p.Wait()
			p.Close()
		}
	}
	batch() // warm-up: runtime threads, console plumbing
	batch() // second warm-up: the runtime may still grow its thread pool
	before := currentHandleCount(t)
	batch()
	after := currentHandleCount(t)
	// A per-exec leak of one handle would show up as ~20 here; the runtime's
	// thread/console noise stays well below that.
	if delta := int(after - before); delta > 10 {
		t.Fatalf("steady-state handle count grew by %d over 20 pipe execs (%d -> %d)", delta, before, after)
	}
}

func currentHandleCount(t *testing.T) uint32 {
	t.Helper()
	// GetProcessHandleCount is not exposed by x/sys; the same number comes from
	// NtQueryInformationProcess with the handle-count class.
	var info struct {
		HandleCount  uint32
		PointerCount uint32
	}
	err := windows.NtQueryInformationProcess(windows.CurrentProcess(), windows.ProcessHandleCount,
		unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), nil)
	if err != nil {
		t.Fatalf("NtQueryInformationProcess: %v", err)
	}
	return info.HandleCount
}
