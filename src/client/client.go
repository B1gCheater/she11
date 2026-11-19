// Package client implements the target-machine agent: pure outbound,
// zero runtime files, never exits on error.
package client

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"she11/src/shared/dnsx"
	"she11/src/shared/frame"
	"she11/src/shared/gen"
	"she11/src/shared/identity"
	"she11/src/shared/logx"
	"she11/src/shared/pipex"
	"she11/src/shared/proto"
	"she11/src/shared/ptyx"
	"she11/src/shared/shell"
	"she11/src/shared/transport"
)

const (
	heartbeatInterval = 15 * time.Second
	heartbeatMissMax  = 6               // 90s without an ack; a transient loss must not kill a live session
	keepaliveAfter    = 2 * time.Second // min runtime before fast-redial throttling
)

// clientCaps reports the execution features this build supports. The list is
// additive: a relay or commander that does not understand a string ignores
// it, and absent means "not supported".
func clientCaps() []string {
	return []string{
		proto.CapExecPipe,
		proto.CapExecCwdEnv,
		proto.CapExecSplitStderr,
	}
}

// Client carries the embedded configuration and runtime state.
type Client struct {
	cfg    gen.ConfigData
	resv   *dnsx.Resolver
	tlsCfg *tls.Config
	id     string
	fp     string
	host   string
	user   string
	shells []string

	sessMu   sync.Mutex
	sessN    int
	opens    map[string]pendingOpen
	stream   map[string]pendingStream
	sessions map[string]*clientSession
}

type clientSession struct {
	ctl    *frame.Control
	ctx    context.Context
	cancel context.CancelFunc
	pty    *ptyx.Session
}

type pendingOpen struct {
	o   proto.OpenSession
	ctl *frame.Control
	ctx context.Context
}

type pendingStream struct {
	stm transport.Stream
	ctl *frame.Control
}

// New prepares the agent.
func New(cfg gen.ConfigData) (*Client, error) {
	tcfg, err := transport.BuildPinnedClientTLS(cfg.ServerCertPEM)
	if err != nil {
		return nil, err
	}
	secret, err := identity.MachineSecret()
	if err != nil {
		secret = fmt.Appendf(nil, "%s|%d", runtime.GOOS, time.Now().UnixNano())
	}
	if cfg.LoopDelaySec <= 0 {
		cfg.LoopDelaySec = 60
	}
	c := &Client{
		cfg:      cfg,
		tlsCfg:   tcfg,
		id:       identity.ClientID(secret),
		fp:       identity.FingerprintHex(secret),
		shells:   shell.DetectAll(),
		opens:    map[string]pendingOpen{},
		stream:   map[string]pendingStream{},
		sessions: map[string]*clientSession{},
	}
	c.host, _ = os.Hostname()
	if u, err := user.Current(); err == nil {
		c.user = u.Username
	}
	c.resv = &dnsx.Resolver{
		Plain:       cfg.DNS,
		DoH:         cfg.DoH,
		Timeout:     time.Duration(cfg.DNSTimeoutSec) * time.Second,
		AllowSystem: cfg.DNSAllowFallback,
	}
	return c, nil
}

// Run starts all supervisors and blocks forever.
func (c *Client) Run() {
	go c.pairingLoop()
	var wg sync.WaitGroup
	for _, ep := range c.cfg.UDPServers {
		wg.Add(1)
		go func(ep string) { defer wg.Done(); c.supervisor(ep, "udp") }(ep)
	}
	for _, ep := range c.cfg.TCPServers {
		wg.Add(1)
		go func(ep string) { defer wg.Done(); c.supervisor(ep, "tcp") }(ep)
	}
	wg.Wait() // supervisors never return; kept for shutdown symmetry
}

// endpoints produces dial candidates for host:port.
func (c *Client) candidates(hostport string) ([]string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	// IP literal?
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip != nil {
		return []string{net.JoinHostPort(ip.String(), port)}, nil
	}
	// Domain: resolve.
	v4, v6, err := c.resv.Lookup(context.Background(), host)
	if err != nil {
		return nil, err
	}
	// skip v6 when the host has no routed v6 (no pointless attempts)
	if len(v6) > 0 && !c.hasIPv6() {
		v6 = nil
	}
	var cands []string
	add := func(ips []net.IP) {
		for _, a := range ips {
			cands = append(cands, net.JoinHostPort(a.String(), port))
		}
	}
	switch c.cfg.IPProtocol {
	case "ipv4":
		add(v4)
	case "ipv6":
		add(v6)
	case "prefer-ipv6":
		add(v6)
		add(v4)
	default: // prefer-ipv4
		add(v4)
		add(v6)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("no dial candidates for %s", hostport)
	}
	return cands, nil
}

func (c *Client) hasIPv6() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.To4() == nil && ip.IsGlobalUnicast() {
				return true
			}
		}
	}
	return false
}

// supervisor dials and maintains ONE (server, transport).
func (c *Client) supervisor(hostport, proto string) {
	loop := time.Duration(c.cfg.LoopDelaySec) * time.Second
	hs := time.Duration(c.cfg.HandshakeTimeoutS) * time.Second
	if hs <= 0 {
		hs = 5 * time.Second
	}
	i := 0
	var lastStart time.Time
	for {
		i++
		if i > 1 {
			wait := loop
			if time.Since(lastStart) < keepaliveAfter {
				wait = time.Second // connection died immediately: throttle
			}
			logx.Printf("[%s/%s] round %d: cooldown %s", proto, hostport, i, wait)
			time.Sleep(wait)
		}
		cands, err := c.candidates(hostport)
		if err != nil {
			logx.Printf("[%s/%s] resolve failed: %v", proto, hostport, err)
			continue
		}
		dialer, err := transport.NewDialer(proto, c.tlsCfg)
		if err != nil {
			logx.Printf("[%s/%s] dialer: %v", proto, hostport, err)
			continue
		}
		conn := dialRace(dialer, cands, hs)
		if conn == nil {
			logx.Printf("[%s/%s] all candidates failed (hs=%s)", proto, hostport, hs)
			continue
		}
		lastStart = time.Now()
		logx.Printf("[%s/%s] transport connected via %s, registering", proto, hostport, conn.RemoteAddr())
		if err := c.runConnection(conn, hs); err != nil {
			logx.Printf("[%s/%s] registration failed via %s: %v", proto, hostport, conn.RemoteAddr(), err)
		}
	}
}

func dialRace(dialer transport.Dialer, cands []string, hs time.Duration) transport.Conn {
	ctx, cancel := context.WithTimeout(context.Background(), hs)
	defer cancel()
	if len(cands) == 1 {
		conn, err := dialer.Dial(ctx, cands[0])
		if err != nil {
			logx.Printf("dial %s failed: %v", cands[0], err)
			return nil
		}
		return conn
	}
	type res struct {
		conn transport.Conn
		err  error
	}
	// Unbuffered handoff ensures that any successful connection arriving
	// after a winner (or cancellation) is closed rather than leaked.
	ch := make(chan res)
	for _, cd := range cands {
		go func(cd string) {
			conn, err := dialer.Dial(ctx, cd)
			if err != nil {
				logx.Printf("dial %s failed: %v", cd, err)
			}
			select {
			case ch <- res{conn, err}:
			case <-ctx.Done():
				if conn != nil {
					conn.CloseWithError(0, "unused dial candidate")
				}
			}
		}(cd)
	}
	for range cands {
		select {
		case r := <-ch:
			if r.err == nil && r.conn != nil {
				return r.conn
			}
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

func (c *Client) register(conn transport.Conn, timeout time.Duration) (control *frame.Control, err error) {
	// A live transport does not guarantee the peer will acknowledge HELLO.
	// Close just this connection so its supervisor can retry independently.
	timer := time.AfterFunc(timeout, func() { _ = conn.CloseWithError(0, "registration timeout") })
	defer func() {
		if !timer.Stop() {
			control, err = nil, fmt.Errorf("registration exceeded %s: %w", timeout, context.DeadlineExceeded)
		}
	}()
	st, err := conn.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("open control stream: %w", err)
	}
	ctl := frame.NewControl(st)

	hello := proto.Hello{
		Role:        "client",
		Magic:       c.cfg.Magic,
		ClientID:    c.id,
		Fingerprint: c.fp,
		Hostname:    c.host,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Username:    c.user,
		Shells:      c.shells,
		MaxShell:    c.cfg.MaxShell,
		Caps:        clientCaps(),
	}
	if err := ctl.Send(proto.THello, hello); err != nil {
		return nil, fmt.Errorf("send HELLO: %w", err)
	}
	env, err := ctl.Recv()
	if err != nil {
		return nil, fmt.Errorf("read HELLO_ACK: %w", err)
	}
	if env.Type != proto.THelloAck {
		return nil, fmt.Errorf("expected HELLO_ACK, got %q", env.Type)
	}
	ack, err := proto.Decode[proto.HelloAck](env)
	if err != nil {
		return nil, fmt.Errorf("decode HELLO_ACK: %w", err)
	}
	logx.Printf("hello-ack from server %q", ack.ServerName)
	return ctl, nil
}

// runConnection serves a registered relay until it disconnects.
func (c *Client) runConnection(conn transport.Conn, timeout time.Duration) error {
	defer conn.CloseWithError(0, "client done")
	ctl, err := c.register(conn, timeout)
	if err != nil {
		return err
	}
	var hbMiss atomic.Int32
	stop := make(chan struct{})
	go c.heartbeatLoop(conn, ctl, stop, &hbMiss)
	go c.controlLoop(ctl, conn.Context(), &hbMiss)
	defer c.cleanupConnection(ctl)

	for {
		stm, err := conn.AcceptStream(conn.Context())
		if err != nil {
			logx.Printf("[%s] connection ended: %v", conn.RemoteAddr(), err)
			close(stop)
			return nil
		}
		go c.handleReverseStream(stm, ctl)
	}
}

// cleanupConnection never disturbs pairing or active sessions on another relay.
func (c *Client) cleanupConnection(ctl *frame.Control) {
	c.sessMu.Lock()
	var streams []transport.Stream
	for id, pending := range c.opens {
		if pending.ctl == ctl {
			delete(c.opens, id)
		}
	}
	for id, st := range c.stream {
		if st.ctl == ctl {
			delete(c.stream, id)
			streams = append(streams, st.stm)
		}
	}
	for _, session := range c.sessions {
		if session.ctl == ctl {
			session.cancel()
		}
	}
	c.sessMu.Unlock()
	for _, stm := range streams {
		stm.Close()
	}
}

func (c *Client) heartbeatLoop(conn transport.Conn, ctl *frame.Control, stop chan struct{}, miss *atomic.Int32) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := ctl.Send(proto.THeartbeat, proto.Heartbeat{ActiveSessions: c.sessionCount()}); err != nil {
				logx.Printf("heartbeat send failed: %v — closing connection", err)
				_ = conn.CloseWithError(0, "heartbeat write failed")
				return
			}
			if miss.Add(1) >= heartbeatMissMax {
				logx.Printf("heartbeat lost: closing connection")
				_ = conn.CloseWithError(0, "no heartbeat ack")
				return
			}
		}
	}
}

func (c *Client) controlLoop(ctl *frame.Control, ctx context.Context, hbMiss *atomic.Int32) {
	for {
		env, err := ctl.Recv()
		if err != nil {
			return
		}
		switch env.Type {
		case proto.THeartbeatAck:
			hbMiss.Store(0)
		case proto.TOpenSession:
			o, err := proto.Decode[proto.OpenSession](env)
			if err != nil {
				continue
			}
			c.onOpenSession(o, ctl, ctx)
		case proto.TResize:
			m, err := proto.Decode[proto.Resize](env)
			if err == nil {
				c.resizeSession(m.SessionID, m.Cols, m.Rows)
			}
		case proto.TClose:
			m, err := proto.Decode[proto.CloseMsg](env)
			if err == nil {
				c.killSession(m.SessionID)
			}
		case proto.TGoingAway:
			logx.Printf("server going away, will redial")
		}
	}
}

func (c *Client) sessionCount() int {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sessN
}

// handleReverseStream reads the 8-byte sid header and stashes the stream
// for the pairing dispatcher (one owner only — no double dispatch).
func (c *Client) handleReverseStream(stm transport.Stream, ctl *frame.Control) {
	var raw [8]byte
	if _, err := io.ReadFull(stm, raw[:]); err != nil {
		stm.Close()
		return
	}
	sid := string(raw[:])
	c.sessMu.Lock()
	c.stream[sid] = pendingStream{stm: stm, ctl: ctl}
	logx.Printf("reverse stream sid=%s arrived stm=%p", sid, stm)
	c.sessMu.Unlock()
	notifyPairing()
}

func (c *Client) onOpenSession(o proto.OpenSession, ctl *frame.Control, ctx context.Context) {
	c.sessMu.Lock()
	c.opens[o.SessionID] = pendingOpen{o: o, ctl: ctl, ctx: ctx}
	c.sessMu.Unlock()
	logx.Printf("OPEN_SESSION sid=%s stashed", o.SessionID)
	notifyPairing()
}

// pairingNotify wakes the single dispatcher.
var pairingCh = make(chan struct{}, 1)

func notifyPairing() {
	select {
	case pairingCh <- struct{}{}:
	default:
	}
}

// pairingLoop is THE single dispatcher: exactly-once pairing of open+stream
// on the same connection.
func (c *Client) pairingLoop() {
	for range pairingCh {
		for {
			c.sessMu.Lock()
			var po pendingOpen
			var ps pendingStream
			var sid string
			var session *clientSession
			var limited bool
			for id := range c.opens {
				if s, ok := c.stream[id]; ok && s.ctl == c.opens[id].ctl {
					sid, po, ps = id, c.opens[id], s
					delete(c.opens, id)
					delete(c.stream, id)
					limited = c.cfg.MaxShell > 0 && c.sessN >= c.cfg.MaxShell
					if !limited && po.ctx.Err() == nil {
						ctx, cancel := context.WithCancel(po.ctx)
						session = &clientSession{ctl: po.ctl, ctx: ctx, cancel: cancel}
						c.sessions[sid] = session
						c.sessN++ // Reserve before spawning; concurrent OPENs cannot exceed the cap.
					}
					break
				}
			}
			c.sessMu.Unlock()
			if sid == "" {
				break
			}
			if session == nil {
				ps.stm.Close()
				if limited {
					_ = po.ctl.Send(proto.TSessionReject, proto.SessionReject{
						SessionID: sid, Code: proto.CodeSessionLimit,
						Reason: fmt.Sprintf("session limit reached (%d)", c.cfg.MaxShell)})
				}
				continue
			}
			logx.Printf("sid=%s paired, spawning session", sid)
			go c.startSession(po.o, ps.stm, po.ctl, session)
		}
	}
}

func (c *Client) startSession(o proto.OpenSession, stm transport.Stream, ctl *frame.Control, state *clientSession) {
	defer c.unregisterSession(o.SessionID)
	if state.ctx.Err() != nil {
		stm.Close()
		return
	}
	spec, err := shell.Resolve(o.Shell)
	if err != nil {
		stm.Close()
		_ = ctl.Send(proto.TSessionReject, proto.SessionReject{
			SessionID: o.SessionID, Code: proto.CodeShellUnavailable,
			Reason: fmt.Sprintf("%v; available: %s", err, strings.Join(c.shells, ", "))})
		return
	}
	switch o.Mode {
	case proto.ModeExecPipe:
		c.runPipeSession(o, stm, ctl, state, spec)
	case proto.ModeExec, proto.ModePTY, "":
		c.runPTYSession(o, stm, ctl, state, spec)
	default:
		// An old client treated every mode except "exec" as interactive and
		// would happily start a shell here. A new one must refuse: the
		// commander would be decoding tagged frames from a terminal stream.
		stm.Close()
		_ = ctl.Send(proto.TSessionReject, proto.SessionReject{
			SessionID: o.SessionID, Code: proto.CodeProtoMismatch,
			Reason: fmt.Sprintf("unsupported session mode %q", o.Mode)})
	}
}

// runPTYSession runs the interactive/legacy-exec path on a pseudo terminal.
func (c *Client) runPTYSession(o proto.OpenSession, stm transport.Stream, ctl *frame.Control, state *clientSession, spec shell.Spec) {
	cols, rows := o.Cols, o.Rows
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	env := append(os.Environ(),
		"TERM="+termOfDefault(o.Term),
		"SHE11_SESSION="+o.SessionID,
		"SHE11_OPERATOR="+o.Operator)
	if o.Operator == "cli" {
		// The display belongs to the commander, not this machine. Clear a
		// locally inherited COLORTERM when the commander did not advertise it.
		env = append(env, "COLORTERM="+o.ColorTerm)
	}
	args := spec.Args()
	if o.Mode == proto.ModeExec && o.Command != "" {
		args = spec.Exec(o.Command)
	}
	sess, err := ptyx.Start(spec.Path, args, env, cols, rows)
	if err != nil {
		stm.Close()
		_ = ctl.Send(proto.TSessionReject, proto.SessionReject{
			SessionID: o.SessionID, Code: proto.CodeNoShell,
			Reason: fmt.Sprintf("spawn %s: %v", spec.Name, err)})
		return
	}
	c.registerSession(o.SessionID, sess)
	defer sess.Close()
	// Cancellation must terminate the process, not just stop relaying its bytes.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-state.ctx.Done():
			sess.Kill()
		case <-watchDone:
		}
	}()

	if err := ctl.Send(proto.TSessionAccept, proto.SessionAccept{SessionID: o.SessionID}); err != nil {
		sess.Kill()
		stm.Close()
		return
	}
	logx.Printf("session %s started: %s (%s)", o.SessionID, spec.Name, o.Mode)

	inCh := make(chan []byte, 16) // terminal-query answers -> pty input
	done := make(chan struct{}, 2)
	ptyDrained := make(chan struct{})
	inputDone := make(chan struct{})
	stopInput := make(chan struct{})
	tr := sessionResponder(o)
	go func() { // pty -> stm (+terminal responses)
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				for _, resp := range tr.Feed(buf[:n]) {
					select {
					case inCh <- resp:
					default:
					}
				}
				if _, werr := stm.Write(buf[:n]); werr != nil {
					logx.Printf("session %s pty->stm write err: %v", o.SessionID, werr)
					break
				}
			}
			if err != nil {
				break
			}
		}
		close(ptyDrained)
	}()
	go func() { // stm -> pty (user input + terminal answers)
		defer func() { done <- struct{}{} }()
		defer close(inputDone)
		type rd struct {
			b   []byte
			err error
		}
		rdCh := make(chan rd, 1)
		readNext := func() {
			buf := make([]byte, 32*1024)
			n, err := stm.Read(buf)
			rdCh <- rd{b: buf[:n], err: err}
		}
		go readNext()
		for {
			select {
			case <-stopInput:
				return
			case resp := <-inCh:
				if _, werr := sess.Write(resp); werr != nil {
					logx.Printf("session %s termresp write err: %v", o.SessionID, werr)
					return
				}
			case r := <-rdCh:
				if r.err != nil {
					return
				}
				if len(r.b) > 0 {
					if _, werr := sess.Write(r.b); werr != nil {
						logx.Printf("session %s stm->pty write err: %v", o.SessionID, werr)
						return
					}
				}
				go readNext()
			}
		}
	}()
	// EOF from commander, relay loss, or an explicit CLOSE all kill the shell.
	go func() {
		select {
		case <-inputDone:
			sess.Kill()
		case <-sess.Done():
		case <-watchDone:
		}
	}()
	code := sess.Wait()
	// drain all pty output BEFORE announcing exit, so late bytes reach the
	// commander before it closes the stream (ordering contract of EXIT).
	// On Windows, conpty delivers all output before the process handle
	// signals, so a short grace suffices; on Unix, pty EOF is immediate.
	select {
	case <-ptyDrained:
	case <-time.After(500 * time.Millisecond):
	}
	// ConPTY Read can remain blocked after process exit. Close the PTY before
	// waiting for its reader, while keeping the transport open to drain bytes.
	sess.Close()
	<-ptyDrained
	close(stopInput)
	<-done
	<-done
	// EXIT acknowledges a fully reclaimed shell, so a caller can immediately
	// replace it without transiently exhausting the concurrency limit.
	c.unregisterSession(o.SessionID)
	_ = ctl.Send(proto.TExit, proto.ExitMsg{SessionID: o.SessionID, Code: code})
	stm.Close()
	logx.Printf("session %s exited code=%d", o.SessionID, code)
}

// Pipe frame tags. One stream carries both channels; the relay stays a byte
// copier and never has to understand them.
const (
	pipeTagStdout = 0x01
	pipeTagStderr = 0x02
	pipeMaxChunk  = 32 * 1024 // well under the 64 KiB frame ceiling
)

// runPipeSession runs one command without a pseudo terminal: separate pipes,
// tagged frames, and no ConPTY creation at all.
func (c *Client) runPipeSession(o proto.OpenSession, stm transport.Stream, ctl *frame.Control, state *clientSession, spec shell.Spec) {
	reject := func(code int, format string, args ...interface{}) {
		stm.Close()
		_ = ctl.Send(proto.TSessionReject, proto.SessionReject{
			SessionID: o.SessionID, Code: code, Reason: fmt.Sprintf(format, args...)})
	}
	if o.Command == "" {
		reject(proto.CodeProtoMismatch, "exec_pipe requires a command")
		return
	}
	if o.Cwd != "" {
		st, err := os.Stat(o.Cwd)
		if err != nil {
			reject(proto.CodeExecCwd, "cwd %q: %v", o.Cwd, err)
			return
		}
		if !st.IsDir() {
			reject(proto.CodeExecCwd, "cwd %q is not a directory", o.Cwd)
			return
		}
	}
	env, err := pipeEnv(o.Env, o.SessionID, o.Operator)
	if err != nil {
		reject(proto.CodeExecEnv, "%v", err)
		return
	}
	proc, err := pipex.Start(spec.Path, spec.Exec(o.Command), env, o.Cwd)
	if err != nil {
		reject(proto.CodeNoShell, "spawn %s: %v", spec.Name, err)
		return
	}
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-state.ctx.Done():
			proc.Kill()
		case <-watchDone:
		}
	}()
	if err := ctl.Send(proto.TSessionAccept, proto.SessionAccept{SessionID: o.SessionID}); err != nil {
		proc.Kill()
		proc.Close()
		stm.Close()
		return
	}
	logx.Printf("pipe session %s started: %s", o.SessionID, spec.Name)

	// The commander sends no stdin for a pipe session, but this side of the
	// stream must still be drained. A QUIC stream is only released once both
	// directions are closed, so unread streams accumulate against the
	// connection's incoming-stream limit and eventually the relay's next
	// OpenStream fails with "too many open streams".
	go func() { _, _ = io.Copy(io.Discard, stm) }()

	// Frames are written by two readers, so writes are serialized: an
	// interleaved header would corrupt both channels.
	var writeMu sync.Mutex
	writeFrame := func(tag byte, p []byte) error {
		var hdr [5]byte
		hdr[0] = tag
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := stm.Write(hdr[:]); err != nil {
			return err
		}
		_, err := stm.Write(p)
		return err
	}
	var readers sync.WaitGroup
	drain := func(tag byte, r io.Reader) {
		defer readers.Done()
		buf := make([]byte, pipeMaxChunk)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if werr := writeFrame(tag, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	readers.Add(2)
	go drain(pipeTagStdout, proc.Stdout)
	go drain(pipeTagStderr, proc.Stderr)
	// Both channels reached EOF, so every byte is already on the wire.
	readers.Wait()
	code := proc.Wait()
	proc.Close()
	// Release the slot before EXIT, so a caller can immediately reuse it.
	c.unregisterSession(o.SessionID)
	_ = ctl.Send(proto.TExit, proto.ExitMsg{SessionID: o.SessionID, Code: code})
	stm.Close()
	logx.Printf("pipe session %s exited code=%d", o.SessionID, code)
}

// pipeEnv merges KEY=VALUE overrides into the client's environment. The
// parent process is never modified, and PATH survives: replacing the whole
// environment would break every command that resolves helpers by name.
func pipeEnv(overrides []string, sessionID, operator string) ([]string, error) {
	merged := append([]string(nil), os.Environ()...)
	index := make(map[string]int, len(merged))
	for i, kv := range merged {
		key := kv
		if j := strings.IndexByte(kv, '='); j >= 0 {
			key = kv[:j]
		}
		index[envKey(key)] = i
	}
	set := func(key, value string) {
		entry := key + "=" + value
		if i, ok := index[envKey(key)]; ok {
			merged[i] = entry
			return
		}
		index[envKey(key)] = len(merged)
		merged = append(merged, entry)
	}
	for _, kv := range overrides {
		j := strings.IndexByte(kv, '=')
		if j <= 0 {
			return nil, fmt.Errorf("invalid env entry %q (want KEY=VALUE)", kv)
		}
		key, value := kv[:j], kv[j+1:]
		if strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("env entry %q contains NUL", key)
		}
		set(key, value)
	}
	// The pipe backend is not a terminal, so TERM is deliberately not set.
	set("SHE11_SESSION", sessionID)
	set("SHE11_OPERATOR", operator)
	return merged, nil
}

// envKey folds the key on Windows, where environment names are
// case-insensitive: PATH and Path must not become two entries.
func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

func termOfDefault(term string) string {
	if term != "" {
		return term
	}
	if runtime.GOOS == "windows" {
		return "xterm-256color"
	}
	return os.Getenv("TERM")
}

// session registry helpers
func (c *Client) registerSession(sid string, s *ptyx.Session) {
	c.sessMu.Lock()
	if state := c.sessions[sid]; state != nil {
		state.pty = s
	}
	c.sessMu.Unlock()
}

func (c *Client) unregisterSession(sid string) {
	c.sessMu.Lock()
	if state := c.sessions[sid]; state != nil {
		state.cancel()
		delete(c.sessions, sid)
		c.sessN--
	}
	c.sessMu.Unlock()
}

func (c *Client) resizeSession(sid string, cols, rows int) {
	c.sessMu.Lock()
	state := c.sessions[sid]
	var s *ptyx.Session
	if state != nil {
		s = state.pty
	}
	c.sessMu.Unlock()
	if s != nil {
		_ = s.Resize(cols, rows)
	}
}

func (c *Client) killSession(sid string) {
	c.sessMu.Lock()
	if state := c.sessions[sid]; state != nil {
		state.cancel()
	}
	c.sessMu.Unlock()
}
