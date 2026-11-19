// Package transport abstracts the two wire transports (QUIC over UDP,
// TLS 1.3 + yamux over TCP) behind one Conn/Stream interface.
package transport

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
)

// ALPN identifies the she11 protocol version.
const ALPN = "she11/1"

// Stream is one multiplexed bidirectional byte stream.
type Stream interface {
	io.ReadWriteCloser
}

// Conn is one multiplexed connection (QUIC conn or yamux session).
type Conn interface {
	OpenStream() (Stream, error)
	AcceptStream(ctx context.Context) (Stream, error)
	CloseWithError(code uint64, desc string) error
	Context() context.Context
	RemoteAddr() net.Addr
	LocalAddr() net.Addr
}

// Dialer dials a concrete host:port.
type Dialer interface {
	Dial(ctx context.Context, addr string) (Conn, error)
}

// ListenerHandle accepts connections.
type ListenerHandle interface {
	Accept(ctx context.Context) (Conn, error)
	Addr() net.Addr
	Close() error
}

// Listen creates a listener of the given proto ("udp" | "tcp").
func Listen(proto, addr string, tlsCfg *tls.Config) (ListenerHandle, error) {
	if !strings.Contains(addr, ":") {
		addr = ":" + addr // bare port = dual-stack on all NICs
	}
	switch proto {
	case "udp":
		warnUDPBufferOnce()
		return listenQUIC(addr, tlsCfg)
	case "tcp":
		return listenTCP(addr, tlsCfg)
	}
	return nil, errUnknownProto(proto)
}

// NewDialer builds a Dialer for the given proto.
func NewDialer(proto string, tlsCfg *tls.Config) (Dialer, error) {
	switch proto {
	case "udp":
		warnUDPBufferOnce()
		return newQUICDialer(tlsCfg)
	case "tcp":
		return newTCPDialer(tlsCfg)
	}
	return nil, errUnknownProto(proto)
}
