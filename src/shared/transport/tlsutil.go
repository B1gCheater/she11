package transport

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/quic-go/quic-go"
)

// BuildPinnedClientTLS builds a client-side tls.Config that pins the
// embedded server certificate: exact DER match + validity + self-signature.
// This is cert pinning, not CA-path validation — the embedded cert IS the
// single trust anchor (rotation = recompile, by design).
func BuildPinnedClientTLS(certPEM string) (*tls.Config, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, errors.New("embedded server cert: bad PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("embedded server cert parse: %w", err)
	}
	return &tls.Config{
		NextProtos:         []string{ALPN},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // verification is done manually below (strict pin)
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("peer presented no certificate")
			}
			peer := cs.PeerCertificates[0]
			if !bytes.Equal(peer.Raw, cert.Raw) {
				return errors.New("peer certificate does not match the pinned certificate")
			}
			now := time.Now()
			if now.Before(peer.NotBefore) || now.After(peer.NotAfter) {
				return errors.New("peer certificate is expired or not yet valid")
			}
			if err := peer.CheckSignatureFrom(cert); err != nil {
				return errors.New("peer certificate signature invalid")
			}
			return nil
		},
	}, nil
}

// BuildServerTLS loads the server certificate + key from files.
func BuildServerTLS(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert/key: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{ALPN},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.NoClientCert, // identity is app-level (magic / password)
	}, nil
}

// QUICConfig returns the terminal-friendly QUIC parameters:
// keepalive beats NAT idle timers; no 0-RTT (replay); no aggressive CC.
func QUICConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:     90 * time.Second,
		KeepAlivePeriod:    20 * time.Second,
		MaxIncomingStreams: 1024,
		EnableDatagrams:    false,
		Allow0RTT:          false,
	}
}

// ServerTLS returns the shared server tls.Config (used by both planes).
func ServerTLS(certFile, keyFile string) (*tls.Config, error) {
	return BuildServerTLS(certFile, keyFile)
}
