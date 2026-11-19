package client

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"she11/src/shared/frame"
	"she11/src/shared/proto"
	"she11/src/shared/transport"
)

type closeProbe struct{ closed bool }

func (s *closeProbe) Read(p []byte) (int, error)  { return 0, io.EOF }
func (s *closeProbe) Write(p []byte) (int, error) { return len(p), nil }
func (s *closeProbe) Close() error                { s.closed = true; return nil }

func TestCleanupOnlyAffectsFailedConnection(t *testing.T) {
	a, b := &frame.Control{}, &frame.Control{}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()
	streamA, streamB := &closeProbe{}, &closeProbe{}
	c := &Client{sessN: 2,
		opens:    map[string]pendingOpen{"a": {ctl: a}, "b": {ctl: b}},
		stream:   map[string]pendingStream{"a": {ctl: a, stm: streamA}, "b": {ctl: b, stm: streamB}},
		sessions: map[string]*clientSession{"a": {ctl: a, ctx: ctxA, cancel: cancelA}, "b": {ctl: b, ctx: ctxB, cancel: cancelB}},
	}
	c.cleanupConnection(a)
	if ctxA.Err() == nil || ctxB.Err() != nil {
		t.Fatal("wrong connection's session canceled")
	}
	if !streamA.closed || streamB.closed {
		t.Fatal("wrong connection's stream closed")
	}
	if len(c.opens) != 1 || c.opens["b"].ctl != b || len(c.stream) != 1 || c.stream["b"].ctl != b {
		t.Fatal("other relay pairing state was lost")
	}
	// Cleanup requests cancellation; the session worker owns releasing its slot.
	if c.sessionCount() != 2 {
		t.Fatal("slot released before worker stopped")
	}
	c.unregisterSession("a")
	c.unregisterSession("a")
	if c.sessionCount() != 1 {
		t.Fatal("slot not released exactly once")
	}
}

var _ transport.Stream = (*closeProbe)(nil)

type dialProbe func(context.Context, string) (transport.Conn, error)

func (f dialProbe) Dial(ctx context.Context, addr string) (transport.Conn, error) {
	return f(ctx, addr)
}

func TestDialRaceAllFailuresReturn(t *testing.T) {
	dialer := dialProbe(func(context.Context, string) (transport.Conn, error) { return nil, errors.New("unavailable") })
	done := make(chan transport.Conn, 1)
	go func() { done <- dialRace(dialer, []string{"one", "two"}, time.Second) }()
	select {
	case conn := <-done:
		if conn != nil {
			t.Fatal("unexpected connection")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("all failed candidates left dialRace stuck")
	}
}

func TestDialRaceDeadlineEndsHungCandidates(t *testing.T) {
	dialer := dialProbe(func(ctx context.Context, _ string) (transport.Conn, error) { <-ctx.Done(); return nil, ctx.Err() })
	done := make(chan transport.Conn, 1)
	go func() { done <- dialRace(dialer, []string{"one", "two"}, 20*time.Millisecond) }()
	select {
	case conn := <-done:
		if conn != nil {
			t.Fatal("unexpected connection")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("dialRace ignored handshake deadline")
	}
}

type registrationConn struct {
	transport.Conn
	stream net.Conn
}

func (c registrationConn) OpenStream() (transport.Stream, error) { return c.stream, nil }
func (c registrationConn) CloseWithError(uint64, string) error   { return c.stream.Close() }

func TestStalledRegistrationDoesNotBlockOtherRelay(t *testing.T) {
	c := &Client{id: "dual-relay-client"}
	stalled, stalledPeer := net.Pipe()
	defer stalled.Close()
	defer stalledPeer.Close()
	stalledResult := make(chan error, 1)
	go func() {
		_, err := c.register(registrationConn{stream: stalled}, 200*time.Millisecond)
		stalledResult <- err
	}()
	_ = stalledPeer.SetDeadline(time.Now().Add(2 * time.Second))
	if env, err := frame.NewControl(stalledPeer).Recv(); err != nil || env.Type != proto.THello {
		t.Fatalf("B did not receive HELLO: %v, %v", env, err)
	}
	// B accepted the transport but never replies to HELLO. A still registers,
	// and its control stream remains usable after B's timeout.
	healthy, healthyPeer := net.Pipe()
	defer healthy.Close()
	defer healthyPeer.Close()
	_ = healthyPeer.SetDeadline(time.Now().Add(2 * time.Second))
	heartbeat := make(chan error, 1)
	go func() {
		ctl := frame.NewControl(healthyPeer)
		if _, err := ctl.Recv(); err != nil {
			heartbeat <- err
			return
		}
		if err := ctl.Send(proto.THelloAck, proto.HelloAck{ServerName: "A"}); err != nil {
			heartbeat <- err
			return
		}
		env, err := ctl.Recv()
		if err == nil && env.Type != proto.THeartbeat {
			err = errors.New("expected heartbeat on A")
		}
		heartbeat <- err
	}()
	ctl, err := c.register(registrationConn{stream: healthy}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stalledResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("B registration error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B registration never returned to its supervisor")
	}
	if err := ctl.Send(proto.THeartbeat, proto.Heartbeat{}); err != nil {
		t.Fatal(err)
	}
	if err := <-heartbeat; err != nil {
		t.Fatal(err)
	}
}
