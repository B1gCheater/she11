// Package commander implements the operator console: list, probe,
// interactive shells, exec and MCP mode.
package commander

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"she11/src/shared/frame"
	"she11/src/shared/gen"
	"she11/src/shared/proto"
	"she11/src/shared/transport"
)

// Commander is the runtime console state.
type Commander struct {
	cfg     Config
	tlsCfg  *tls.Config
	timeout time.Duration
}

// New builds the commander.
func New(cfg Config) (*Commander, error) {
	cfg.Defaults()
	tcfg, err := transport.BuildPinnedClientTLS(gen.Config.ServerCertPEM)
	if err != nil {
		return nil, err
	}
	c := &Commander{cfg: cfg, tlsCfg: tcfg, timeout: 10 * time.Second}
	if cfg.General.Timeout > 0 {
		c.timeout = time.Duration(cfg.General.Timeout) * time.Second
	}
	return c, nil
}

// liveConn is one authed server connection.
type liveConn struct {
	conn      transport.Conn
	ctl       *frame.Control
	ctlStream transport.Stream
	name      string
	// features are the relay features from hello_ack. They gate fields the
	// relay has to forward; an old relay silently drops unknown JSON fields,
	// so sending them blind would be data loss rather than an error.
	features map[string]bool
}

func (lc *liveConn) hasFeature(name string) bool {
	return lc != nil && lc.features[name]
}

// boundControl bounds a control exchange, including transports whose Stream
// does not expose deadlines. A timed-out connection is never reused.
func boundControl(lc *liveConn, timeout time.Duration) func() {
	if ds, ok := lc.ctlStream.(interface{ SetDeadline(time.Time) error }); ok {
		_ = ds.SetDeadline(time.Now().Add(timeout))
	}
	timer := time.AfterFunc(timeout, func() { _ = lc.conn.CloseWithError(0, "control timeout") })
	return func() {
		timer.Stop()
		if ds, ok := lc.ctlStream.(interface{ SetDeadline(time.Time) error }); ok {
			_ = ds.SetDeadline(time.Time{})
		}
	}
}

func (c *Commander) controlRequest(lc *liveConn, request string, payload interface{}, response string) (*proto.Envelope, error) {
	stop := boundControl(lc, c.timeout)
	defer stop()
	if err := lc.ctl.Send(request, payload); err != nil {
		return nil, fmt.Errorf("send %s: %w", request, err)
	}
	env, err := lc.ctl.Recv()
	if err != nil {
		return nil, fmt.Errorf("receive %s: %w", response, err)
	}
	if env.Type != response {
		return nil, fmt.Errorf("expected %s, got %s", response, env.Type)
	}
	return env, nil
}

// dialAuth connects to a server, does HELLO+AUTH. protoKind: "udp"|"tcp"|"auto".
func (c *Commander) dialAuth(addr, password, protoKind string) (*liveConn, error) {
	protos := []string{protoKind}
	if protoKind == "auto" {
		protos = []string{"udp", "tcp"}
	}
	var lastErr error
	for _, p := range protos {
		dialer, err := transport.NewDialer(p, c.tlsCfg)
		if err != nil {
			lastErr = err
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		conn, err := dialer.Dial(ctx, addr)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		// OpenStream and the application handshake must also be bounded after
		// the transport dial's context has been released.
		stopHandshake := boundControl(&liveConn{conn: conn}, c.timeout)
		st, err := conn.OpenStream()
		if err != nil {
			stopHandshake()
			conn.CloseWithError(0, "x")
			lastErr = err
			continue
		}
		ctl := frame.NewControl(st)
		if err := ctl.Send(proto.THello, proto.Hello{Role: "commander"}); err != nil {
			stopHandshake()
			conn.CloseWithError(0, "x")
			lastErr = err
			continue
		}
		env, err := ctl.Recv()
		if err != nil || env.Type != proto.THelloAck {
			stopHandshake()
			conn.CloseWithError(0, "x")
			if err != nil {
				lastErr = fmt.Errorf("server handshake failed: %w", err)
			} else {
				lastErr = fmt.Errorf("expected hello_ack, got %s", env.Type)
			}
			continue
		}
		ack, err := proto.Decode[proto.HelloAck](env)
		if err != nil {
			stopHandshake()
			conn.CloseWithError(0, "x")
			lastErr = fmt.Errorf("invalid hello_ack: %w", err)
			continue
		}
		if err := ctl.Send(proto.TAuth, proto.Auth{Password: password}); err != nil {
			stopHandshake()
			conn.CloseWithError(0, "x")
			lastErr = err
			continue
		}
		env, err = ctl.Recv()
		if err != nil || env.Type != proto.TAuthResult {
			stopHandshake()
			conn.CloseWithError(0, "x")
			if err != nil {
				lastErr = fmt.Errorf("auth handshake failed: %w", err)
			} else {
				lastErr = fmt.Errorf("expected auth_result, got %s", env.Type)
			}
			continue
		}
		ar, err := proto.Decode[proto.AuthResult](env)
		stopHandshake()
		if err != nil {
			conn.CloseWithError(0, "x")
			lastErr = fmt.Errorf("invalid auth_result: %w", err)
			continue
		}
		if !ar.OK {
			conn.CloseWithError(0, "x")
			return nil, fmt.Errorf("auth failed: %s", ar.Reason)
		}
		features := make(map[string]bool, len(ack.Features))
		for _, f := range ack.Features {
			features[f] = true
		}
		return &liveConn{conn: conn, ctl: ctl, ctlStream: st, name: ack.ServerName, features: features}, nil
	}
	return nil, lastErr
}

// ResolveServers returns the effective server list: inline (pass:addr) or config.
func (c *Commander) ResolveServers(inline []string) []ServerConf {
	if len(inline) > 0 {
		var out []ServerConf
		for i, x := range inline {
			pass, addr, ok := strings.Cut(x, ":")
			if !ok {
				continue
			}
			out = append(out, ServerConf{Name: fmt.Sprintf("s%d", i+1), Addr: addr, Password: pass})
		}
		return out
	}
	return c.cfg.Servers
}

// Connects to all servers concurrently, returning per-server results.
func (c *Commander) connectAll(servers []ServerConf, protoKind string) ([]*liveConn, []error) {
	conns := make([]*liveConn, len(servers))
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lc, err := c.dialAuth(servers[i].Addr, servers[i].Password, protoKind)
			if err != nil {
				errs[i] = err
				return
			}
			conns[i] = lc
		}(i)
	}
	wg.Wait()
	return conns, errs
}

// probeServers counts a server as healthy only after an authenticated status RPC.
func (c *Commander) probeServers(servers []ServerConf) ([]checkRow, int) {
	conns, errs := c.connectAll(servers, c.cfg.General.Transport)
	reachable := 0
	rows := make([]checkRow, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rows[i] = checkRow{name: servers[i].Name, addr: servers[i].Addr, status: "error"}
			lc := conns[i]
			if lc == nil {
				if errs[i] != nil {
					rows[i].status = errs[i].Error()
				}
				return
			}
			defer lc.conn.CloseWithError(0, "done")
			env, err := c.controlRequest(lc, proto.TServerInfoReq, proto.ServerInfoReq{}, proto.TServerInfo)
			if err != nil {
				rows[i].status = err.Error()
				return
			}
			info, err := proto.Decode[proto.ServerInfo](env)
			if err != nil {
				rows[i].status = "invalid server_info: " + err.Error()
				return
			}
			rows[i].status, rows[i].clients, rows[i].uptime = "ok", info.ClientCount, info.UptimeSec
		}(i)
	}
	wg.Wait()
	for _, row := range rows {
		if row.status == "ok" {
			reachable++
		}
	}
	return rows, reachable
}

// renderCheckText lays out probe rows in the same pipe-separated style as
// --list-client. The STATUS column carries only the short ok/error mark so
// a long error message cannot blow up the layout; the full error text of
// every failed relay prints below the table, one line each, mirroring the
// "server error:" lines of list-client.
func renderCheckText(rows []checkRow, reachable, total int) string {
	cells := make([][]string, 0, len(rows))
	var failed []checkRow
	for _, r := range rows {
		if r.status == "ok" {
			cells = append(cells, []string{r.name, r.addr, r.status,
				fmt.Sprintf("%d", r.clients), fmt.Sprintf("%ds", r.uptime)})
			continue
		}
		failed = append(failed, r)
		// Clients and uptime are unknown (not zero) on a failed probe.
		cells = append(cells, []string{r.name, r.addr, "error", "-", "-"})
	}
	table, _ := renderTable(
		[]string{"SERVER", "ADDR", "STATUS", "CLIENTS", "UPTIME"},
		cells,
		[]int{28, 32, 10, 10, 12},
		stdoutWidth(),
	)
	var b strings.Builder
	b.WriteString(table)
	fmt.Fprintf(&b, "%d of %d reachable\n", reachable, total)
	for _, r := range failed {
		fmt.Fprintf(&b, "server error: %s (%s): %s\n", r.name, r.addr, r.status)
	}
	return b.String()
}

// CheckServer probes health of the given servers and prints a table.
func (c *Commander) CheckServer(servers []ServerConf, jsonOut bool) int {
	rows, reachable := c.probeServers(servers)
	if jsonOut {
		printJSONCheck(rows, reachable)
	} else {
		fmt.Print(renderCheckText(rows, reachable, len(servers)))
	}
	if reachable == 0 {
		return 3
	}
	return 0
}

type serverFailure struct {
	Name  string `json:"name"`
	Addr  string `json:"addr"`
	Error string `json:"error"`
}

type clientSnapshot struct {
	server  ServerConf
	lc      *liveConn
	clients []proto.ClientInfo
	err     error
}

func closeSnapshots(snapshots []clientSnapshot, keep *liveConn) {
	for _, snapshot := range snapshots {
		if snapshot.lc != nil && snapshot.lc != keep {
			_ = snapshot.lc.conn.CloseWithError(0, "done")
		}
	}
}

// clientSnapshots keeps successful connections open so selection and OPEN use
// the same authenticated relay. No command is replayed on a different relay.
func (c *Commander) clientSnapshots(servers []ServerConf, protoKind string) []clientSnapshot {
	if protoKind == "" {
		protoKind = c.cfg.General.Transport
	}
	conns, errs := c.connectAll(servers, protoKind)
	snapshots := make([]clientSnapshot, len(servers))
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snapshot := clientSnapshot{server: servers[i], lc: conns[i], err: errs[i]}
			defer func() { snapshots[i] = snapshot }()
			if snapshot.lc == nil {
				return
			}
			env, err := c.controlRequest(snapshot.lc, proto.TListClients, proto.ListClients{}, proto.TClientList)
			if err != nil {
				snapshot.err = err
				return
			}
			list, err := proto.Decode[proto.ClientList](env)
			if err != nil {
				snapshot.err = fmt.Errorf("invalid client_list: %w", err)
				return
			}
			snapshot.clients = list.Clients
		}(i)
	}
	wg.Wait()
	return snapshots
}

func mergeSnapshots(snapshots []clientSnapshot) ([]proto.ClientInfo, []serverFailure, error) {
	merged := make([]proto.ClientInfo, 0)
	failures := make([]serverFailure, 0)
	seen := map[string]bool{}
	succeeded := 0
	for _, snapshot := range snapshots {
		if snapshot.err != nil {
			failures = append(failures, serverFailure{snapshot.server.Name, snapshot.server.Addr, snapshot.err.Error()})
			continue
		}
		succeeded++
		for _, cl := range snapshot.clients {
			if !seen[cl.ID] {
				seen[cl.ID] = true
				merged = append(merged, cl)
				continue
			}
			// The same device can register through several relays; the union
			// of advertised caps is the honest view of what it supports.
			for i := range merged {
				if merged[i].ID == cl.ID {
					merged[i].Caps = unionStrings(merged[i].Caps, cl.Caps)
					break
				}
			}
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	if succeeded == 0 {
		if len(failures) == 0 {
			return merged, failures, codedErrorf(errCodeNoRelay, "no servers configured")
		}
		return merged, failures, codedErrorf(errCodeRelayDown, "no server roster available: %s", failureSummary(failures))
	}
	return merged, failures, nil
}

func failureSummary(failures []serverFailure) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, fmt.Sprintf("%s (%s): %s", failure.Name, failure.Addr, failure.Error))
	}
	return strings.Join(parts, "; ")
}

// unionStrings appends values from b that a does not already contain.
func unionStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	for _, s := range a {
		seen[s] = true
	}
	out := a
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (c *Commander) queryClients(servers []ServerConf, protoKind string) ([]proto.ClientInfo, []serverFailure, error) {
	snapshots := c.clientSnapshots(servers, protoKind)
	defer closeSnapshots(snapshots, nil)
	return mergeSnapshots(snapshots)
}

// ListClients prints the merged client roster, including partial relay errors.
func (c *Commander) ListClients(servers []ServerConf, jsonOut bool) int {
	merged, failures, err := c.queryClients(servers, c.cfg.General.Transport)
	if jsonOut {
		printJSONClients(merged, failures, err)
	} else {
		rows := make([][]string, 0, len(merged))
		for _, cl := range merged {
			username := cl.Username
			if cl.OS == "windows" {
				if i := strings.LastIndexByte(username, '\\'); i >= 0 && i+1 < len(username) {
					username = username[i+1:]
				}
			}
			rows = append(rows, []string{cl.ID, cl.Hostname, username,
				cl.OS + "/" + cl.Arch, cl.Via, strings.Join(cl.Shells, ",")})
		}
		table, _ := renderTable(
			[]string{"ID", "HOSTNAME", "USER", "OS/ARCH", "VIA", "SHELLS"},
			rows,
			[]int{10, 28, 28, 16, 12, 36},
			stdoutWidth(),
		)
		fmt.Print(table)
		if len(merged) == 1 {
			fmt.Println("1 client")
		} else {
			fmt.Printf("%d clients\n", len(merged))
		}
		for _, failure := range failures {
			fmt.Printf("server error: %s (%s): %s\n", failure.Name, failure.Addr, failure.Error)
		}
		if err != nil {
			fmt.Printf("error: %s\n", err)
		}
	}
	if err != nil {
		return 3
	}
	return 0
}

// resolveClient maps a target against the whole deduplicated fleet, prioritizing
// an exact ID before aliases. Prefixes are never resolved one relay at a time.
func resolveClient(clients []proto.ClientInfo, target string) (string, error) {
	if target == "" {
		return "", codedErrorf(errCodeInvalidArgument, "client target is empty")
	}
	for _, cl := range clients {
		if cl.ID == target {
			return cl.ID, nil
		}
	}
	var cands []string
	for _, cl := range clients {
		if strings.HasPrefix(cl.ID, target) || cl.ObservedAddr == target || cl.Hostname == target {
			cands = append(cands, cl.ID)
		}
	}
	switch len(cands) {
	case 1:
		return cands[0], nil
	case 0:
		return "", codedErrorf(errCodeTargetNotFound, "no client matches %q", target)
	default:
		return "", codedErrorf(errCodeTargetAmbiguous, "ambiguous target %q: %s", target, strings.Join(cands, ", "))
	}
}

// pickTarget locates the requested client and retains its first configured
// healthy relay. Duplicate sightings of the same client are a single target.
func (c *Commander) pickTarget(servers []ServerConf, target, protoKind string) (*liveConn, string, error) {
	lc, id, _, err := c.pickTargetInfo(servers, target, protoKind)
	return lc, id, err
}

// pickTargetInfo also reports the target's advertised capabilities, which is
// what lets exec choose a backend without ever asking an old client to do
// something it would misinterpret.
func (c *Commander) pickTargetInfo(servers []ServerConf, target, protoKind string) (*liveConn, string, map[string]bool, error) {
	snapshots := c.clientSnapshots(servers, protoKind)
	clients, failures, err := mergeSnapshots(snapshots)
	if err != nil {
		closeSnapshots(snapshots, nil)
		return nil, "", nil, err
	}
	id, err := resolveClient(clients, target)
	if err != nil {
		closeSnapshots(snapshots, nil)
		if len(failures) > 0 {
			return nil, "", nil, fmt.Errorf("%w; unavailable servers: %s", err, failureSummary(failures))
		}
		return nil, "", nil, err
	}
	caps := map[string]bool{}
	for _, cl := range clients {
		if cl.ID == id {
			for _, cap := range cl.Caps {
				caps[cap] = true
			}
			break
		}
	}
	for _, snapshot := range snapshots {
		if snapshot.err != nil {
			continue
		}
		for _, cl := range snapshot.clients {
			if cl.ID == id {
				closeSnapshots(snapshots, snapshot.lc)
				return snapshot.lc, id, caps, nil
			}
		}
	}
	closeSnapshots(snapshots, nil)
	return nil, "", nil, codedErrorf(errCodeClientOffline, "no relay for client %q", id)
}

// waitFor reads control frames until one of the given types appears.
func waitFor(ctl *frame.Control, types ...string) (*proto.Envelope, error) {
	for {
		env, err := ctl.Recv()
		if err != nil {
			return nil, err
		}
		for _, t := range types {
			if env.Type == t {
				return env, nil
			}
		}
	}
}

// openSession performs OPEN and waits for the paired data stream.
func (c *Commander) openSession(lc *liveConn, o proto.Open) (transport.Stream, string, error) {
	// Each session uses its own commander connection. Accept exactly one
	// paired stream and let cancellation close any stream that arrives late.
	streamCh := make(chan struct {
		sid string
		stm transport.Stream
		err error
	})
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	stop := boundControl(lc, c.timeout)
	defer stop()
	go func() {
		stm, err := lc.conn.AcceptStream(ctx)
		var sid string
		if err == nil {
			if ds, ok := stm.(interface{ SetReadDeadline(time.Time) error }); ok {
				_ = ds.SetReadDeadline(time.Now().Add(c.timeout))
			}
			var raw [8]byte
			_, err = io.ReadFull(stm, raw[:])
			if err != nil {
				_ = stm.Close()
			} else {
				sid = string(raw[:])
				if ds, ok := stm.(interface{ SetReadDeadline(time.Time) error }); ok {
					_ = ds.SetReadDeadline(time.Time{})
				}
			}
		}
		select {
		case streamCh <- struct {
			sid string
			stm transport.Stream
			err error
		}{sid, stm, err}:
		case <-ctx.Done():
			// Only a successfully accepted stream may be closed; a failed
			// AcceptStream returns nil and closing it would panic.
			if err == nil && stm != nil {
				_ = stm.Close()
			}
		}
	}()

	if err := lc.ctl.Send(proto.TOpen, o); err != nil {
		return nil, "", err
	}
	env, err := waitFor(lc.ctl, proto.TOpenResult)
	if err != nil {
		return nil, "", err
	}
	res, err := proto.Decode[proto.OpenResult](env)
	if err != nil {
		return nil, "", fmt.Errorf("invalid open_result: %w", err)
	}
	if !res.OK {
		return nil, "", &openError{code: res.Code, reason: res.Reason}
	}
	if len(res.SessionID) != 8 {
		return nil, "", fmt.Errorf("invalid session id in open_result")
	}
	// wait for the data stream
	select {
	case p := <-streamCh:
		if p.err != nil {
			return nil, "", fmt.Errorf("accept session stream: %w", p.err)
		}
		if p.sid == res.SessionID {
			return p.stm, res.SessionID, nil
		}
		_ = p.stm.Close()
		return nil, "", fmt.Errorf("session stream mismatch (%s != %s)", p.sid, res.SessionID)
	case <-ctx.Done():
		return nil, "", fmt.Errorf("timeout waiting for session stream")
	}
}

// openError is a structured OPEN failure.
type openError struct {
	code   int
	reason string
}

func (e *openError) Error() string { return e.reason }
