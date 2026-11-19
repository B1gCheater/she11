package client

import (
	"bytes"

	"she11/src/shared/proto"
)

// Human CLI sessions have a real terminal at the other end. Only headless
// sessions need synthetic answers. Operator is already forwarded by old relays,
// so this fix also works through a relay that predates COLORTERM forwarding.
func sessionResponder(o proto.OpenSession) *termResp {
	if o.Mode == proto.ModePTY && o.Operator == "cli" {
		return nil
	}
	return &termResp{}
}

// termResp is a minimal terminal query responder. Interactive shells (fish,
// vim, htop) send device-attrs / color queries and BLOCK until the terminal
// answers; a bare PTY never answers, so we emulate the answers a real
// terminal (xterm-ish) would give. Everything else passes through untouched.
type termResp struct {
	state int // 0 plain | 1 esc | 2 csi | 3 osc | 4 dcs
	buf   []byte
	param []byte
}

// Feed scans a chunk of pty output and returns responses to inject into the
// pty input (in order). Feed is not reentrant-safe by design (owned by the
// pty→stream pump goroutine).
func (t *termResp) Feed(chunk []byte) [][]byte {
	if t == nil {
		return nil
	}
	var out [][]byte
	for _, b := range chunk {
		switch t.state {
		case 0:
			if b == 0x1b {
				t.state = 1
				t.buf = []byte{b}
			}
		case 1:
			t.buf = append(t.buf, b)
			switch b {
			case '[':
				t.state = 2
				t.param = nil
			case ']':
				t.state = 3
				t.param = nil
			case 'P':
				t.state = 4
				t.param = nil
			default:
				t.state = 0
			}
		case 2: // CSI
			t.buf = append(t.buf, b)
			if b >= 0x40 && b <= 0x7e { // final byte
				seq := append([]byte{}, t.buf...)
				t.state = 0
				if r := t.csi(seq); r != nil {
					out = append(out, r)
				}
			} else if (b >= 0x30 && b <= 0x3f) || b == '=' || b == '>' || b == '?' {
				t.param = append(t.param, b)
			}
		case 3: // OSC
			t.buf = append(t.buf, b)
			if (b == 0x07) ||
				(b == '\\' && len(t.buf) >= 2 && t.buf[len(t.buf)-2] == 0x1b) {
				seq := append([]byte{}, t.buf...)
				t.state = 0
				if r := t.osc(seq); r != nil {
					out = append(out, r)
				}
			}
		case 4: // DCS ... ST
			t.buf = append(t.buf, b)
			if b == '\\' && len(t.buf) >= 2 && t.buf[len(t.buf)-2] == 0x1b {
				t.state = 0
			}
		}
	}
	return out
}

var (
	respEmpty = []byte{}
	respDA1   = []byte("\x1b[?62;1;2;4;6;8;9;15;22c") // xterm-ish primary DA
	respXT    = []byte("\x1bP>|she11(0.1)\x1b\\")     // XTVERSION name
	respBG    = []byte("\x1b]11;rgb:1f1f/1f1f/1f1f\x1b\\")
	respFG    = []byte("\x1b]10;rgb:f0f0/f0f0/f0f0\x1b\\")
)

func (t *termResp) csi(seq []byte) []byte {
	if len(seq) < 3 {
		return nil
	}
	if seq[1] != '[' {
		return nil
	}
	final := seq[len(seq)-1]
	params := seq[2 : len(seq)-1]
	switch {
	case final == 'c' && (len(params) == 0 || bytes.Equal(params, []byte{'0'})):
		// primary DA query: ESC[c  (fish sends ESC[0c)
		return respDA1
	case final == 'c' && len(params) == 1 && params[0] == '>': // secondary DA: ESC[>c
		return []byte("\x1b[>0;276;0c")
	case final == 'q' && bytes.Equal(params, []byte{'>', '0'}): // XTVERSION: ESC[>0q
		return respXT
	case final == 'n' && bytes.Equal(params, []byte{'6'}): // CPR: ESC[6n
		return []byte("\x1b[1;1R")
	case final == 'R' || final == 'r':
		// locale/mode replies from the app itself: pass through
		return nil
	}
	return nil
}

func (t *termResp) osc(seq []byte) []byte {
	if len(seq) < 3 || seq[1] != ']' {
		return nil
	}
	// strip ST/BEL terminator
	body := seq[2 : len(seq)-1]
	for len(body) > 0 && (body[len(body)-1] == 0x07 || body[len(body)-1] == '\\' || body[len(body)-1] == 0x1b) {
		body = body[:len(body)-1]
	}
	switch {
	case bytes.HasPrefix(body, []byte("11;?")): // background color query
		return respBG
	case bytes.HasPrefix(body, []byte("10;?")): // foreground color query
		return respFG
	}
	return respEmpty
}
