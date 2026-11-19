// Package frame implements the length-prefixed framing of the control
// stream: varint length + JSON envelope.
package frame

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"she11/src/shared/proto"
)

// MaxSize is the largest accepted frame (1 MiB).
const MaxSize = 1 << 20

// sendTimeout bounds one control-frame write. A stalled peer must never block
// a caller (or a server-wide lock) forever; 10s is well above a healthy RTT.
const sendTimeout = 10 * time.Second

// writeDeadliner is implemented by QUIC and yamux streams. Plain io.Writers
// (tests, pipes) simply go without a deadline.
type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

// ErrTooLarge is returned when a frame exceeds MaxSize.
var ErrTooLarge = errors.New("frame exceeds max size")

// Control is a framed, concurrent-safe control channel over one stream.
type Control struct {
	br  *bufio.Reader
	w   io.Writer
	mu  sync.Mutex
	seq uint64
}

// NewControl wraps an in+out stream.
func NewControl(rw io.ReadWriteCloser) *Control {
	return &Control{br: bufio.NewReader(rw), w: rw}
}

// Send marshals and writes one message. Safe for concurrent use.
func (c *Control) Send(msgType string, msg interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	env, err := proto.NewEnvelope(msgType, c.seq, msg)
	if err != nil {
		return err
	}
	return c.sendLocked(env)
}

// SendRaw writes a prebuilt envelope (used for pass-through relay).
func (c *Control) SendRaw(env *proto.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendLocked(env)
}

// sendLocked writes one frame under a bounded deadline. A failed or partial
// write leaves the framing unusable, so the stream is closed: the peer then
// fails fast instead of parsing garbage.
func (c *Control) sendLocked(env *proto.Envelope) error {
	if d, ok := c.w.(writeDeadliner); ok {
		_ = d.SetWriteDeadline(time.Now().Add(sendTimeout))
		defer d.SetWriteDeadline(time.Time{})
	}
	err := c.writeEnv(env)
	if err != nil && !errors.Is(err, ErrTooLarge) {
		if closer, ok := c.w.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return err
}

func (c *Control) writeEnv(env *proto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(b) > MaxSize {
		return ErrTooLarge
	}
	var hdr [binary.MaxVarintLen32]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	if _, err := c.w.Write(hdr[:n]); err != nil {
		return err
	}
	_, err = c.w.Write(b)
	return err
}

// Recv reads one envelope. Not safe for concurrent use (one reader goroutine).
func (c *Control) Recv() (*proto.Envelope, error) {
	ln, err := binary.ReadUvarint(c.br)
	if err != nil {
		return nil, err
	}
	if ln > MaxSize {
		return nil, ErrTooLarge
	}
	buf := make([]byte, ln)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return nil, err
	}
	var env proto.Envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Flush is a no-op placeholder for symmetry (buffered writes go out on Send).
func (c *Control) Flush() {
	if f, ok := c.w.(*bufio.Writer); ok {
		_ = f.Flush()
	}
}
