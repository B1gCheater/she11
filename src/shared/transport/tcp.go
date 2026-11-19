package transport

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

func yamuxCfg() *yamux.Config {
	return &yamux.Config{
		AcceptBacklog:          64,
		EnableKeepAlive:        true,
		KeepAliveInterval:      20 * time.Second,
		ConnectionWriteTimeout: 10 * time.Second,
		MaxStreamWindowSize:    8 << 20,
		LogOutput:              io.Discard,
	}
}

type tcpConn struct {
	sess   *yamux.Session
	ctx    context.Context
	cancel context.CancelFunc
}

// OpenStream returns an untyped nil on failure: a typed nil would satisfy a
// "stm != nil" check and panic when closed.
func (c *tcpConn) OpenStream() (Stream, error) {
	s, err := c.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c *tcpConn) AcceptStream(ctx context.Context) (Stream, error) {
	type res struct {
		s   Stream
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := c.sess.AcceptStream()
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || r.s == nil {
			return nil, r.err
		}
		return r.s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

func (c *tcpConn) CloseWithError(code uint64, desc string) error {
	c.cancel()
	return c.sess.Close()
}

func (c *tcpConn) Context() context.Context { return c.ctx }
func (c *tcpConn) RemoteAddr() net.Addr     { return c.sess.RemoteAddr() }
func (c *tcpConn) LocalAddr() net.Addr      { return c.sess.LocalAddr() }

type tcpDialer struct{ tls *tls.Config }

func newTCPDialer(tlsCfg *tls.Config) (Dialer, error) { return &tcpDialer{tls: tlsCfg}, nil }

func (d *tcpDialer) Dial(ctx context.Context, addr string) (Conn, error) {
	nd := net.Dialer{}
	nc, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(nc, d.tls)
	if err := tc.HandshakeContext(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	sess, err := yamux.Client(tc, yamuxCfg())
	if err != nil {
		tc.Close()
		return nil, err
	}
	cctx, cancel := context.WithCancel(context.Background())
	return &tcpConn{sess: sess, ctx: cctx, cancel: cancel}, nil
}

type tcpListenerHandle struct {
	ln  net.Listener
	tls *tls.Config
}

func listenTCP(addr string, tlsCfg *tls.Config) (ListenerHandle, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &tcpListenerHandle{ln: ln, tls: tlsCfg}, nil
}

func (l *tcpListenerHandle) Accept(ctx context.Context) (Conn, error) {
	for {
		type res struct {
			c   net.Conn
			err error
		}
		ch := make(chan res, 1)
		go func() {
			c, err := l.ln.Accept()
			ch <- res{c, err}
		}()
		var nc net.Conn
		select {
		case r := <-ch:
			if r.err != nil {
				return nil, r.err
			}
			nc = r.c
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		tc := tls.Server(nc, l.tls)
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			nc.Close()
			continue
		}
		sess, err := yamux.Server(tc, yamuxCfg())
		if err != nil {
			tc.Close()
			continue
		}
		cctx, ccancel := context.WithCancel(context.Background())
		return &tcpConn{sess: sess, ctx: cctx, cancel: ccancel}, nil
	}
}

func (l *tcpListenerHandle) Addr() net.Addr { return l.ln.Addr() }
func (l *tcpListenerHandle) Close() error   { return l.ln.Close() }
