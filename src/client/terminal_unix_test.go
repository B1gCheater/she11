//go:build !windows

package client

import (
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"she11/src/shared/frame"
	"she11/src/shared/proto"
	"she11/src/shared/shell"
)

// Exercise the real PTY pumps. The child acts like tmux: query device attributes,
// read the terminal answer, then wait for stray replies that would reach a shell.
func TestPTYHumanHasOneTerminalAnswer(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for PTY probe")
	}
	const script = `import os, tty, select, time
tty.setraw(0)
os.write(1, b'\x1b[c')
data = b''
end = time.monotonic() + 0.5
while time.monotonic() < end:
    if select.select([0], [], [], max(0, end-time.monotonic()))[0]:
        data += os.read(0, 4096)
os.write(1, ('ANSWER=' + data.hex() + ';COLOR=' + os.getenv('COLORTERM', '') + '\n').encode())
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state := &clientSession{ctx: ctx, cancel: cancel}
	c := &Client{sessN: 1, sessions: map[string]*clientSession{"probe": state}}
	local, remote := net.Pipe()
	defer remote.Close()
	_ = remote.SetDeadline(time.Now().Add(5 * time.Second))
	ctlLocal, ctlRemote := net.Pipe()
	defer ctlLocal.Close()
	defer ctlRemote.Close()
	go io.Copy(io.Discard, ctlRemote)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runPTYSession(proto.OpenSession{SessionID: "probe", Mode: proto.ModePTY,
			Operator: "cli", Term: "xterm-256color", ColorTerm: "truecolor"},
			local, frame.NewControl(ctlLocal), state,
			shell.Spec{Name: "probe", Path: python, Args: func() []string { return []string{"-c", script} }})
	}()
	var output bytes.Buffer
	buf := make([]byte, 4096)
	answered := false
	for {
		n, readErr := remote.Read(buf)
		output.Write(buf[:n])
		if !answered && bytes.Contains(output.Bytes(), []byte("\x1b[c")) {
			answered = true
			// Actual Windows Terminal primary DA from the reported failure.
			_, err = remote.Write([]byte("\x1b[?61;4;6;7;14;21;22;23;24;28;32;42;52c"))
			if err != nil {
				t.Fatal(err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	<-done
	want := "ANSWER=1b5b3f36313b343b363b373b31343b32313b32323b32333b32343b32383b33323b34323b353263;COLOR=truecolor"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("PTY got duplicate/missing answers or lost color: %q", output.String())
	}
}
