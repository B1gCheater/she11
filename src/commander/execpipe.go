// Package commander: decoding the non-PTY exec data stream.
//
// The stream layout after the 8-byte session id header is:
//
//	tag    uint8    0x01 = stdout, 0x02 = stderr
//	length uint32   big-endian payload length, 1..64 KiB
//	payload [length]byte
//
// The relay copies these bytes without interpreting them, so the only place
// that has to understand the format is this file and the client writer.
package commander

import (
	"bufio"
	"encoding/binary"
	"io"
	"sync/atomic"
)

const (
	execBackendPTY  = "pty"
	execBackendPipe = "pipe"

	pipeTagStdout = 0x01
	pipeTagStderr = 0x02
	pipeFrameMax  = 64 << 10
)

// pipeCapture accumulates the three views of a piped command: arrival order
// (merged, the compatibility field), stdout and stderr separately. Each view
// keeps its newest execOutputLimit bytes; malformed records are counted as
// discarded instead of being shown as output.
type pipeCapture struct {
	merged execOutputBuffer
	stdout execOutputBuffer
	stderr execOutputBuffer
	broken atomic.Int64
}

// readAll consumes frames until EOF. A truncated tail can happen when the
// commander times out mid-frame: its bytes are counted, never reported as
// output, and never treated as a protocol violation that hides earlier data.
func (p *pipeCapture) readAll(r io.Reader) {
	br := bufio.NewReaderSize(r, pipeFrameMax)
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(br, hdr[:1]); err != nil {
			return // clean EOF between frames
		}
		tag := hdr[0]
		got, err := io.ReadFull(br, hdr[1:])
		if err != nil {
			p.broken.Add(int64(1 + got)) // tag byte plus any partial length
			return
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		if (tag != pipeTagStdout && tag != pipeTagStderr) || n == 0 || n > pipeFrameMax {
			p.broken.Add(5)
			return
		}
		payload := make([]byte, n)
		read, err := io.ReadFull(br, payload)
		if err != nil {
			p.broken.Add(int64(read) + 5)
			return
		}
		p.append(tag, payload)
	}
}

func (p *pipeCapture) append(tag byte, b []byte) {
	p.merged.Write(b)
	switch tag {
	case pipeTagStdout:
		p.stdout.Write(b)
	case pipeTagStderr:
		p.stderr.Write(b)
	}
}

// snapshot returns the three views plus the byte count that is missing from
// the merged output. Channel-only truncation is bounded by the same total, so
// the merged count is the honest lower bound to report.
func (p *pipeCapture) snapshot() (merged, stdout, stderr string, discarded int64) {
	merged, mergedDropped := p.merged.Snapshot()
	stdout, _ = p.stdout.Snapshot()
	stderr, _ = p.stderr.Snapshot()
	return merged, stdout, stderr, mergedDropped + p.broken.Load()
}
