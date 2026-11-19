package transport

import (
	"context"
	"crypto/tls"
	"net"

	"github.com/quic-go/quic-go"
)

type quicConn struct{ q *quic.Conn }

func (c *quicConn) OpenStream() (Stream, error) {
	s, err := c.q.OpenStream()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c *quicConn) AcceptStream(ctx context.Context) (Stream, error) {
	// Returning the concrete (*quic.Stream, error) directly would wrap a nil
	// pointer in a non-nil interface on failure, and callers that check
	// "stm != nil" would then call Close on a nil receiver.
	s, err := c.q.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c *quicConn) CloseWithError(code uint64, desc string) error {
	return c.q.CloseWithError(quic.ApplicationErrorCode(code), desc)
}

func (c *quicConn) Context() context.Context { return c.q.Context() }
func (c *quicConn) RemoteAddr() net.Addr     { return c.q.RemoteAddr() }
func (c *quicConn) LocalAddr() net.Addr      { return c.q.LocalAddr() }

type quicDialer struct {
	tls   *tls.Config
	qconf *quic.Config
}

func newQUICDialer(tlsCfg *tls.Config) (Dialer, error) {
	return &quicDialer{tls: tlsCfg, qconf: QUICConfig()}, nil
}

func (d *quicDialer) Dial(ctx context.Context, addr string) (Conn, error) {
	q, err := quic.DialAddr(ctx, addr, d.tls, d.qconf)
	if err != nil {
		return nil, err
	}
	return &quicConn{q: q}, nil
}

type quicListener struct {
	l   *quic.Listener
	tls *tls.Config
}

func listenQUIC(addr string, tlsCfg *tls.Config) (ListenerHandle, error) {
	l, err := quic.ListenAddr(addr, tlsCfg, QUICConfig())
	if err != nil {
		return nil, err
	}
	return &quicListener{l: l, tls: tlsCfg}, nil
}

func (l *quicListener) Accept(ctx context.Context) (Conn, error) {
	c, err := l.l.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &quicConn{q: c}, nil
}

func (l *quicListener) Addr() net.Addr { return l.l.Addr() }
func (l *quicListener) Close() error   { return l.l.Close() }
