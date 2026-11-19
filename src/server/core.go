// Package server implements she11_server: pure relay, registry, auth, audit.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"she11/src/shared/frame"
	"she11/src/shared/logx"
	"she11/src/shared/proto"
	"she11/src/shared/transport"
)

const (
	planeClient    = "client"
	planeCommander = "commander"

	clientOfflineAfter = 120 * time.Second
)

type clientHandle struct {
	info     proto.Hello
	conn     transport.Conn
	ctl      *frame.Control
	observed string
	started  time.Time
	lastSeen atomic.Int64
	active   atomic.Int32
}

type commanderHandle struct {
	conn     transport.Conn
	ctl      *frame.Control
	observed string
	authed   atomic.Bool
}

type relay struct {
	sid        string
	clientStm  transport.Stream
	clientConn transport.Conn
	cmdrStm    transport.Stream
	cmdr       *commanderHandle
	clientCtl  *frame.Control
	dump       *DumpWriter
	shell      string
	done       chan struct{}

	// A session must tell its commander exactly once that it ended, even when
	// the client disappears without an EXIT frame (connection loss, kill -9).
	exitOnce sync.Once
	exitCh   chan struct{}
}

type ipRec struct {
	wrong       int
	bannedUntil time.Time
}

// Server is the relay core.
type Server struct {
	cfg Config
	tls *tls.Config
	log *Logger

	mu      sync.Mutex
	clients map[string][]*clientHandle
	relays  map[string]*relay
	pending map[string]chan *proto.Envelope

	failMu   sync.Mutex
	failByIP map[string]*ipRec

	sessionsTotal atomic.Int32
	started       time.Time
	listeners     []transport.ListenerHandle
}

// New creates the server with a prebuilt server tls.Config.
func New(cfg Config, tlsCfg *tls.Config, logger *Logger) *Server {
	return &Server{
		cfg:      cfg,
		tls:      tlsCfg,
		log:      logger,
		clients:  map[string][]*clientHandle{},
		relays:   map[string]*relay{},
		pending:  map[string]chan *proto.Envelope{},
		failByIP: map[string]*ipRec{},
		started:  time.Now(),
	}
}

// clientCount returns the total number of live client connections.
func (s *Server) clientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, hs := range s.clients {
		n += len(hs)
	}
	return n
}

// lookupClient returns the most recent handle for an id.
func (s *Server) lookupClient(id string) *clientHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs := s.clients[id]
	if len(hs) == 0 {
		return nil
	}
	return hs[len(hs)-1]
}

// Run binds all listeners and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	planes := []struct {
		entries []ListenEntry
		plane   string
	}{
		{s.cfg.Listen.Client, planeClient},
		{s.cfg.Listen.Commander, planeCommander},
	}
	for _, p := range planes {
		for _, e := range p.entries {
			l, err := transport.Listen(e.Proto, e.Addr, s.tls)
			if err != nil {
				s.closeListeners()
				return fmt.Errorf("listen %s/%s: %w", p.plane, e.Addr, err)
			}
			s.listeners = append(s.listeners, l)
			s.log.Printf("listening plane=%s proto=%s addr=%s", p.plane, e.Proto, e.Addr)
			go s.acceptLoop(l, p.plane)
		}
	}
	if len(s.listeners) == 0 {
		return fmt.Errorf("no listen entries configured")
	}
	go s.reaper(ctx)
	<-ctx.Done()
	for _, l := range s.listeners {
		_ = l.Close()
	}
	return nil
}

func (s *Server) closeListeners() {
	for _, l := range s.listeners {
		_ = l.Close()
	}
}

func (s *Server) acceptLoop(l transport.ListenerHandle, plane string) {
	for {
		conn, err := l.Accept(context.Background())
		if err != nil {
			return
		}
		go s.serveConn(conn, plane)
	}
}

func (s *Server) serveConn(conn transport.Conn, plane string) {
	defer conn.CloseWithError(0, "done")
	logx.Printf("conn plane=%s from=%s", plane, conn.RemoteAddr())
	st, err := conn.AcceptStream(conn.Context())
	if err != nil {
		return
	}
	ctl := frame.NewControl(st)
	env, err := ctl.Recv()
	if err != nil || env.Type != proto.THello {
		conn.CloseWithError(1, "expected HELLO")
		return
	}
	hello, err := proto.Decode[proto.Hello](env)
	if err != nil {
		conn.CloseWithError(1, "bad HELLO")
		return
	}
	switch hello.Role {
	case "client":
		if plane != planeClient {
			s.log.Printf("client %s rejected: connected to %s port, expected client port", conn.RemoteAddr(), plane)
			conn.CloseWithError(2, "client on wrong plane")
			return
		}
		s.serveClient(conn, ctl, hello)
	case "commander":
		if plane != planeCommander {
			conn.CloseWithError(2, "commander on wrong plane")
			return
		}
		s.serveCommander(conn, ctl)
	default:
		conn.CloseWithError(1, "unknown role")
	}
}

// ---- client side ----

func (s *Server) magicOK(got string) bool {
	if got == "" {
		return false
	}
	for _, want := range s.cfg.Auth.MagicList {
		if len(want) == len(got) &&
			subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) serveClient(conn transport.Conn, ctl *frame.Control, hello proto.Hello) {
	if !s.magicOK(hello.Magic) {
		s.log.Printf("client %s rejected: bad magic", conn.RemoteAddr())
		conn.CloseWithError(2, "bad magic")
		return
	}
	s.mu.Lock()
	if s.cfg.Limits.MaxClients > 0 {
		n := 0
		for _, hs := range s.clients {
			n += len(hs)
		}
		if n >= s.cfg.Limits.MaxClients {
			s.mu.Unlock()
			s.log.Printf("client %s rejected: server full (max_clients=%d)", conn.RemoteAddr(), s.cfg.Limits.MaxClients)
			conn.CloseWithError(3, "server full")
			return
		}
	}
	ch := &clientHandle{
		info:     hello,
		conn:     conn,
		ctl:      ctl,
		observed: conn.RemoteAddr().String(),
		started:  time.Now(),
	}
	ch.lastSeen.Store(time.Now().Unix())
	hs := s.clients[hello.ClientID]
	if len(hs) >= 4 { // evict oldest standby when too many
		old := hs[0]
		hs = hs[1:]
		go old.conn.CloseWithError(0, "evicted standby")
	}
	s.clients[hello.ClientID] = append(hs, ch)
	s.mu.Unlock()

	s.log.Printf("client online id=%s host=%s os=%s/%s from=%s",
		hello.ClientID, hello.Hostname, hello.OS, hello.Arch, ch.observed)

	if err := ctl.Send(proto.THelloAck, proto.HelloAck{
		ServerName: s.cfg.Identity.Name,
		Features:   []string{proto.FeatureRelayCaps, proto.FeatureOpenCwdEnv},
	}); err != nil {
		goto offline
	}

	for {
		env, err := ctl.Recv()
		if err != nil {
			// The control stream is the first stream on the connection; its
			// failure is what makes a registered client disappear. Log the
			// reason so a "client went offline" report can be explained.
			s.log.Printf("client %s control read failed from=%s: %v", hello.ClientID, ch.observed, err)
			break
		}
		switch env.Type {
		case proto.THeartbeat:
			hb, _ := proto.Decode[proto.Heartbeat](env)
			ch.lastSeen.Store(time.Now().Unix())
			ch.active.Store(int32(hb.ActiveSessions))
			_ = ctl.Send(proto.THeartbeatAck, proto.HeartbeatAck{})
		case proto.TSessionAccept, proto.TSessionReject, proto.TExit:
			s.routeOtherFrame(env)
		}
	}

offline:
	s.mu.Lock()
	hs = s.clients[hello.ClientID]
	for i, h := range hs {
		if h == ch {
			hs = append(hs[:i], hs[i+1:]...)
			break
		}
	}
	if len(hs) == 0 {
		delete(s.clients, hello.ClientID)
	} else {
		s.clients[hello.ClientID] = hs
	}
	// kill relays hosted by this client
	var dead []string
	for sid, r := range s.relays {
		if r.clientCtl == ctl {
			dead = append(dead, sid)
		}
	}
	s.mu.Unlock()
	for _, sid := range dead {
		s.mu.Lock()
		if r, ok := s.relays[sid]; ok {
			r.clientStm.Close()
			r.cmdrStm.Close()
		}
		s.mu.Unlock()
	}
	s.log.Printf("client offline id=%s from=%s", hello.ClientID, ch.observed)
}

// routeOtherFrame delivers client-sent session frames to the waiting OPEN
// flow or forwards EXIT to the relay's commander.
func (s *Server) routeOtherFrame(env *proto.Envelope) {
	var sid string
	switch env.Type {
	case proto.TSessionAccept:
		if m, err := proto.Decode[proto.SessionAccept](env); err == nil {
			sid = m.SessionID
		}
	case proto.TSessionReject:
		if m, err := proto.Decode[proto.SessionReject](env); err == nil {
			sid = m.SessionID
		}
	case proto.TExit:
		if m, err := proto.Decode[proto.ExitMsg](env); err == nil {
			sid = m.SessionID
		}
	}
	if sid == "" {
		return
	}
	var r *relay
	s.mu.Lock()
	if p := s.pending[sid]; p != nil {
		select {
		case p <- env:
		default:
		}
	}
	if rr, ok := s.relays[sid]; ok && env.Type == proto.TExit {
		r = rr
	}
	s.mu.Unlock()
	// Never write to a commander while holding s.mu: one stalled peer would
	// block every client's control loop, including heartbeat acknowledgements.
	if r != nil {
		r.forwardExit(env)
	}
}

// forwardExit delivers at most one EXIT for this session and marks it seen so
// the teardown path does not synthesize a duplicate.
func (r *relay) forwardExit(env *proto.Envelope) {
	r.exitOnce.Do(func() {
		forwardExit(r.cmdr, env)
		close(r.exitCh)
	})
}

// forwardExit relays an EXIT frame to the commander that owns the session.
// Callers may hold s.mu; the write is a control-frame send on an already
// authenticated connection.
func forwardExit(cmdr *commanderHandle, env *proto.Envelope) {
	if cmdr == nil || cmdr.ctl == nil {
		return
	}
	_ = cmdr.ctl.Send(proto.TExit, env.Msg)
}

// ---- commander side ----

func (s *Server) ipOf(conn transport.Conn) string {
	host, _, err := splitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

func (s *Server) isBanned(ip string) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	r := s.failByIP[ip]
	return r != nil && time.Now().Before(r.bannedUntil)
}

func (s *Server) recordFail(ip string) {
	n := s.cfg.Auth.MaxWrongAttempts
	ban := time.Duration(s.cfg.Auth.WrongBanSeconds) * time.Second
	s.failMu.Lock()
	defer s.failMu.Unlock()
	r := s.failByIP[ip]
	if r == nil {
		r = &ipRec{}
		s.failByIP[ip] = r
	}
	r.wrong++
	if r.wrong >= n {
		r.bannedUntil = time.Now().Add(ban)
		r.wrong = 0
		s.log.Printf("commander IP %s banned for %s", ip, ban)
	}
}

func (s *Server) clearFail(ip string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	delete(s.failByIP, ip)
}

func (s *Server) passwordOK(got string) bool {
	want := s.cfg.Auth.CommanderPassword
	// constant-time (equal lengths padded)
	a := make([]byte, len(want))
	b := make([]byte, len(got))
	copy(a, want)
	copy(b, got)
	return subtle.ConstantTimeCompare(a, b) == 1
}

func (s *Server) serveCommander(conn transport.Conn, ctl *frame.Control) {
	ip := s.ipOf(conn)
	if s.isBanned(ip) {
		conn.CloseWithError(4, "banned")
		return
	}
	if s.cfg.Limits.MaxCommander > 0 {
		if s.sessionCount(planeCommander) >= s.cfg.Limits.MaxCommander {
			conn.CloseWithError(5, "too many commanders")
			return
		}
	}
	cmdr := &commanderHandle{conn: conn, ctl: ctl, observed: conn.RemoteAddr().String()}
	if err := ctl.Send(proto.THelloAck, proto.HelloAck{
		ServerName: s.cfg.Identity.Name,
		Features:   []string{proto.FeatureRelayCaps, proto.FeatureOpenCwdEnv},
	}); err != nil {
		return
	}
	// AUTH
	env, err := ctl.Recv()
	if err != nil || env.Type != proto.TAuth {
		conn.CloseWithError(4, "auth required")
		return
	}
	auth, _ := proto.Decode[proto.Auth](env)
	if !s.passwordOK(auth.Password) {
		s.recordFail(ip)
		_ = ctl.Send(proto.TAuthResult, proto.AuthResult{OK: false, Reason: "bad password"})
		conn.CloseWithError(4, "auth failed")
		s.log.Printf("commander auth FAILED from %s", ip)
		return
	}
	s.clearFail(ip)
	_ = ctl.Send(proto.TAuthResult, proto.AuthResult{OK: true})
	cmdr.authed.Store(true)
	s.log.Printf("commander authed from %s", cmdr.observed)

	for {
		env, err := ctl.Recv()
		if err != nil {
			return
		}
		switch env.Type {
		case proto.TListClients:
			s.sendClientList(cmdr)
		case proto.TServerInfoReq:
			s.sendServerInfo(cmdr)
		case proto.THeartbeat:
			_ = ctl.Send(proto.THeartbeatAck, proto.HeartbeatAck{})
		case proto.TOpen:
			if o, err := proto.Decode[proto.Open](env); err == nil {
				go s.handleOpen(cmdr, o)
			}
		case proto.TResize:
			if m, err := proto.Decode[proto.Resize](env); err == nil {
				s.mu.Lock()
				r := s.relays[m.SessionID]
				s.mu.Unlock()
				if r != nil && r.cmdr == cmdr {
					_ = r.clientCtl.Send(proto.TResize, m)
				}
			}
		case proto.TClose:
			if m, err := proto.Decode[proto.CloseMsg](env); err == nil {
				s.mu.Lock()
				r := s.relays[m.SessionID]
				s.mu.Unlock()
				if r != nil && r.cmdr == cmdr {
					_ = r.clientCtl.Send(proto.TClose, m)
					r.clientStm.Close()
					r.cmdrStm.Close()
				}
			}
		}
	}
}

func (s *Server) sessionCount(_ string) int {
	return int(s.sessionsTotal.Load())
}

func (s *Server) sendClientList(cmdr *commanderHandle) {
	s.mu.Lock()
	list := make([]proto.ClientInfo, 0, len(s.clients))
	for _, hs := range s.clients {
		if len(hs) == 0 {
			continue
		}
		ch := hs[len(hs)-1]
		list = append(list, proto.ClientInfo{
			ID:             ch.info.ClientID,
			Hostname:       ch.info.Hostname,
			OS:             ch.info.OS,
			Arch:           ch.info.Arch,
			Username:       ch.info.Username,
			Shells:         ch.info.Shells,
			ObservedAddr:   ch.observed,
			Via:            s.cfg.Identity.Name,
			ConnectedAt:    ch.started.Unix(),
			LastSeen:       ch.lastSeen.Load(),
			ActiveSessions: int(ch.active.Load()),
			MaxShell:       ch.info.MaxShell,
			Caps:           ch.info.Caps,
		})
	}
	s.mu.Unlock()
	_ = cmdr.ctl.Send(proto.TClientList, proto.ClientList{Clients: list, ServerName: s.cfg.Identity.Name})
}

func (s *Server) sendServerInfo(cmdr *commanderHandle) {
	nc := s.clientCount()
	_ = cmdr.ctl.Send(proto.TServerInfo, proto.ServerInfo{
		Name:         s.cfg.Identity.Name,
		UptimeSec:    int64(time.Since(s.started).Seconds()),
		ClientCount:  nc,
		SessionCount: int(s.sessionsTotal.Load()),
		ObservedAddr: cmdr.observed,
		PublicAddr:   s.cfg.Identity.PublicAddr,
	})
}

// ---- relay / OPEN ----

// randSid returns an 8-char hex session id (fits the 8-byte stream header).
func randSid() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleOpen(cmdr *commanderHandle, o proto.Open) {
	ch := s.lookupClient(o.ClientID)
	fail := func(code int, reason string) {
		_ = cmdr.ctl.Send(proto.TOpenResult, proto.OpenResult{OK: false, Code: code, Reason: reason})
	}
	if ch == nil {
		fail(proto.CodeClientOffline, fmt.Sprintf("client %s is offline", o.ClientID))
		return
	}
	if s.cfg.Limits.MaxSessionsTotal > 0 &&
		int(s.sessionsTotal.Load()) >= s.cfg.Limits.MaxSessionsTotal {
		fail(proto.CodeServerBusy, "server session limit reached")
		return
	}
	sid := randSid()

	clientStm, err := ch.conn.OpenStream()
	if err != nil {
		fail(proto.CodeServerBusy, "client stream open failed: "+err.Error())
		return
	}
	if _, err := io.WriteString(clientStm, sid); err != nil {
		clientStm.Close()
		fail(proto.CodeServerBusy, "client stream write failed: "+err.Error())
		return
	}
	cmdrStm, err := cmdr.conn.OpenStream()
	if err != nil {
		clientStm.Close()
		fail(proto.CodeServerBusy, "commander stream open failed: "+err.Error())
		return
	}
	if _, err := io.WriteString(cmdrStm, sid); err != nil {
		clientStm.Close()
		cmdrStm.Close()
		fail(proto.CodeServerBusy, "commander stream write failed")
		return
	}

	// Capacity 4 is not cosmetic: a fast command sends session_accept and EXIT
	// almost back to back. With a one-slot channel the second frame could be
	// dropped before this goroutine consumed the first, losing the exit status
	// while the output had already been delivered.
	pch := make(chan *proto.Envelope, 4)
	s.mu.Lock()
	s.pending[sid] = pch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, sid)
		s.mu.Unlock()
	}()

	if err := ch.ctl.Send(proto.TOpenSession, proto.OpenSession{
		SessionID: sid,
		Shell:     o.Shell,
		Mode:      o.Mode,
		Term:      o.Term,
		ColorTerm: o.ColorTerm,
		Cols:      o.Cols,
		Rows:      o.Rows,
		Command:   o.Command,
		Operator:  o.Operator,
		Cwd:       o.Cwd,
		Env:       o.Env,
	}); err != nil {
		clientStm.Close()
		cmdrStm.Close()
		fail(proto.CodeClientOffline, "client control stream failed")
		return
	}

	timeout := time.Duration(s.cfg.Limits.OpenShellTimeout) * time.Second
	select {
	case env := <-pch:
		switch env.Type {
		case proto.TSessionAccept:
			dump, _ := s.log.SessionDump(o.Shell, sid)
			r := &relay{
				sid:        sid,
				clientStm:  clientStm,
				clientConn: ch.conn,
				cmdrStm:    cmdrStm,
				cmdr:       cmdr,
				clientCtl:  ch.ctl,
				dump:       dump,
				shell:      o.Shell,
				done:       make(chan struct{}),
				exitCh:     make(chan struct{}),
			}
			s.mu.Lock()
			s.relays[sid] = r
			s.mu.Unlock()
			s.sessionsTotal.Add(1)
			s.log.Printf("session open sid=%s client=%s shell=%s operator=%s",
				sid, o.ClientID, o.Shell, o.Operator)
			_ = cmdr.ctl.Send(proto.TOpenResult, proto.OpenResult{OK: true, SessionID: sid})
			go r.run(s)
			// The client may have finished before this session was registered.
			// Any EXIT already queued for the pending channel is delivered now
			// instead of being dropped, which would look like a timeout to the
			// commander even though the command completed.
			for {
				select {
				case early := <-pch:
					if early.Type == proto.TExit {
						r.forwardExit(early)
					}
				default:
					return
				}
			}
		case proto.TSessionReject:
			rej, _ := proto.Decode[proto.SessionReject](env)
			clientStm.Close()
			cmdrStm.Close()
			fail(rej.Code, rej.Reason)
		}
	case <-time.After(timeout):
		clientStm.Close()
		cmdrStm.Close()
		fail(proto.CodeTimeout, fmt.Sprintf("no response from client within %s", timeout))
	}
}

func (r *relay) run(s *Server) {
	sessionDone := make(chan struct{})
	defer func() {
		close(sessionDone)
		s.sessionsTotal.Add(-1)
		s.mu.Lock()
		delete(s.relays, r.sid)
		s.mu.Unlock()
		if r.dump != nil {
			r.dump.Close()
		}
		r.clientStm.Close()
		r.cmdrStm.Close()
		s.log.Printf("session end sid=%s", r.sid)
	}()

	// A dead client connection must close its session streams immediately:
	// otherwise a copy can stay blocked on a half-open stream long after the
	// process is gone, leaving the commander waiting for an EOF or EXIT.
	if r.clientConn != nil {
		go func() {
			select {
			case <-r.clientConn.Context().Done():
				_ = r.clientStm.Close()
				_ = r.cmdrStm.Close()
			case <-sessionDone:
			}
		}()
	}

	done := make(chan struct{}, 2)
	// client -> commander (shell output)
	go func() {
		defer func() { done <- struct{}{} }()
		n, err := io.Copy(io.MultiWriter(r.cmdrStm, dumpWriter{r.dump, "out"}), r.clientStm)
		s.log.Printf("relay %s client->cmdr copied %d bytes err=%v", r.sid, n, err)
		r.cmdrStm.Close()
	}()
	// commander -> client (shell input)
	go func() {
		defer func() { done <- struct{}{} }()
		n, err := io.Copy(io.MultiWriter(r.clientStm, dumpWriter{r.dump, "in"}), r.cmdrStm)
		s.log.Printf("relay %s cmdr->client copied %d bytes err=%v", r.sid, n, err)
		r.clientStm.Close()
	}()
	<-done
	// Close the commander side first: a write to a dead client's control
	// stream can take up to the frame deadline and must not delay the
	// commander's EOF. TClose only asks the shell to stop, so it can run
	// detached and be bounded by that deadline.
	_ = r.cmdrStm.Close()
	_ = r.clientStm.Close()
	if r.clientCtl != nil {
		go func() {
			_ = r.clientCtl.Send(proto.TClose, proto.CloseMsg{SessionID: r.sid})
		}()
	}
	<-done
	// A client that vanished without EXIT (connection loss, kill -9) must not
	// leave the commander waiting forever. Give a real EXIT a short grace, then
	// synthesize one so the CLI or MCP call can finish with a definite end.
	select {
	case <-r.exitCh:
	case <-time.After(500 * time.Millisecond):
		s.log.Printf("session %s: no EXIT from client, synthesizing lost exit", r.sid)
		if env, err := proto.NewEnvelope(proto.TExit, 0, proto.ExitMsg{SessionID: r.sid, Code: -1}); err == nil {
			r.forwardExit(env)
		}
	}
}

func (s *Server) reaper(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-clientOfflineAfter).Unix()
			s.mu.Lock()
			var stale []*clientHandle
			for id, hs := range s.clients {
				var kept []*clientHandle
				for _, ch := range hs {
					if ch.lastSeen.Load() < cutoff {
						stale = append(stale, ch)
					} else {
						kept = append(kept, ch)
					}
				}
				if len(kept) == 0 {
					delete(s.clients, id)
				} else {
					s.clients[id] = kept
				}
			}
			s.mu.Unlock()
			for _, ch := range stale {
				s.log.Printf("client %s offline (heartbeat timeout)", ch.info.ClientID)
				ch.conn.CloseWithError(0, "heartbeat timeout")
			}
		}
	}
}

func splitHostPort(s string) (string, string, error) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i], s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("no port")
}
