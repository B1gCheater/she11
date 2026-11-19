package commander

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// terminalOutput forwards bytes unchanged and observes alternate-screen modes.
// Sending ?1049l unconditionally can restore a stale saved cursor even when
// the remote shell never entered the alternate screen.
type terminalOutput struct {
	mu      sync.Mutex
	w       io.Writer
	closed  bool
	altMode string
	state   byte // plain, ESC, CSI, string, string-ESC
	params  []byte
	osc     bool
}

func (t *terminalOutput) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := t.w.Write(p)
	t.observe(p[:n])
	return n, err
}

func (t *terminalOutput) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	if t.altMode != "" {
		_, _ = fmt.Fprintf(t.w, "\x1b[?%sl", t.altMode)
	}
	// Keep the main-screen cursor where the remote output left it. Start the
	// local prompt on a fresh line, including when remote output lacked a LF.
	_, _ = io.WriteString(t.w, "\x1b[?25h\x1b[0m\r\n")
}

func (t *terminalOutput) observe(p []byte) {
	for _, b := range p {
		// CAN/SUB abort an unfinished control sequence.
		if b == 0x18 || b == 0x1a {
			t.state = 0
			continue
		}
		switch t.state {
		case 0:
			if b == 0x1b {
				t.state = 1
			}
		case 1:
			switch b {
			case '[':
				t.state, t.params = 2, t.params[:0]
			case ']', 'P', '_', '^', 'X':
				t.state, t.osc = 3, b == ']'
			case 0x1b:
			default:
				t.state = 0
			}
		case 2:
			if b == 0x1b {
				t.state = 1
				continue
			}
			if b >= 0x40 && b <= 0x7e {
				if (b == 'h' || b == 'l') && len(t.params) > 0 && t.params[0] == '?' {
					for _, mode := range strings.Split(string(t.params[1:]), ";") {
						switch mode {
						case "47", "1047", "1049":
							if b == 'h' {
								t.altMode = mode
							} else {
								t.altMode = ""
							}
						}
					}
				}
				t.state = 0
			} else if b >= 0x20 {
				if len(t.params) < 128 {
					t.params = append(t.params, b)
				} else {
					t.state = 0
				}
			}
		case 3:
			if b == 0x1b {
				t.state = 4
			} else if b == 0x07 && t.osc {
				t.state = 0
			}
		case 4:
			if b == '\\' || (b == 0x07 && t.osc) {
				t.state = 0
			} else if b != 0x1b {
				t.state = 3
			}
		}
	}
}
