package commander

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"she11/src/shared/frame"
	"she11/src/shared/proto"
	"she11/src/shared/transport"
)

func relayTestTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay-test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := transport.BuildPinnedClientTLS(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	return clientTLS, &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{transport.ALPN}, MinVersion: tls.VersionTLS13,
	}
}

type testRelay struct {
	clients   []proto.ClientInfo
	listMode  string
	infoMode  string
	hangHello bool
	holdExec  bool
	// closeData closes the data stream after the first output without sending
	// EXIT, simulating a relay/client that vanished mid-session.
	closeData bool
	// execFirstOutput is written on the data stream right after the session id
	// and before the relay goes quiet, so a test can prove that bytes produced
	// before a timeout survive instead of being discarded.
	execFirstOutput string
	opens           atomic.Int32
	closeSession    chan string
	// dropFn is filled in by startTestRelay so a test can tear the relay's
	// connections down mid-session and observe a lost-connection outcome.
	dropFn func()
}

// dropConns closes every connection the relay has accepted, simulating a relay
// that disappears while a command is still running. dropFn is assigned
// synchronously by startTestRelay before it returns, so the later read from a
// test goroutine is ordered by that assignment.
func (b *testRelay) dropConns() {
	if b.dropFn != nil {
		b.dropFn()
	}
}

// This fixture speaks the actual TLS/yamux transport and framed protocol; it
// does not execute commands on the host.
func startTestRelay(t *testing.T, serverTLS *tls.Config, name string, behavior *testRelay) ServerConf {
	t.Helper()
	ln, err := transport.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var conns []transport.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.CloseWithError(0, "test done")
				stm, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				ctl := frame.NewControl(stm)
				if _, err := ctl.Recv(); err != nil {
					return
				}
				if behavior.hangHello {
					<-conn.Context().Done()
					return
				}
				if err := ctl.Send(proto.THelloAck, proto.HelloAck{ServerName: name}); err != nil {
					return
				}
				if _, err := ctl.Recv(); err != nil {
					return
				}
				if err := ctl.Send(proto.TAuthResult, proto.AuthResult{OK: true}); err != nil {
					return
				}
				for {
					env, err := ctl.Recv()
					if err != nil {
						return
					}
					switch env.Type {
					case proto.TListClients:
						switch behavior.listMode {
						case "hang":
							<-conn.Context().Done()
							return
						case "malformed":
							_ = ctl.Send(proto.TClientList, "bad list")
						default:
							_ = ctl.Send(proto.TClientList, proto.ClientList{Clients: behavior.clients})
						}
					case proto.TServerInfoReq:
						switch behavior.infoMode {
						case "hang":
							<-conn.Context().Done()
							return
						case "malformed":
							_ = ctl.Send(proto.TServerInfo, "bad status")
						default:
							_ = ctl.Send(proto.TServerInfo, proto.ServerInfo{ClientCount: len(behavior.clients), UptimeSec: 10})
						}
					case proto.TOpen:
						behavior.opens.Add(1)
						if !behavior.holdExec {
							_ = ctl.Send(proto.TOpenResult, proto.OpenResult{Code: proto.CodeClientDenied, Reason: "test rejection"})
							continue
						}
						data, err := conn.OpenStream()
						if err != nil {
							return
						}
						_ = ctl.Send(proto.TOpenResult, proto.OpenResult{OK: true, SessionID: "12345678"})
						_, _ = data.Write([]byte("12345678"))
						if behavior.execFirstOutput != "" {
							// Bytes arrive, then the relay stays silent and
							// never sends EXIT: the command is still running.
							_, _ = data.Write([]byte(behavior.execFirstOutput))
						}
						if behavior.closeData {
							_ = data.Close()
						}
					case proto.TClose:
						closeMsg, _ := proto.Decode[proto.CloseMsg](env)
						behavior.closeSession <- closeMsg.SessionID
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.CloseWithError(0, "test cleanup")
		}
		mu.Unlock()
		wg.Wait()
	})
	behavior.dropFn = func() {
		mu.Lock()
		snapshot := append([]transport.Conn(nil), conns...)
		mu.Unlock()
		for _, conn := range snapshot {
			_ = conn.CloseWithError(0, "test drop")
		}
	}
	return ServerConf{Name: name, Addr: ln.Addr().String(), Password: "test"}
}

func testCommander(clientTLS *tls.Config) *Commander {
	var cfg Config
	cfg.Defaults()
	cfg.General.Transport = "tcp"
	return &Commander{cfg: cfg, tlsCfg: clientTLS, timeout: 400 * time.Millisecond}
}

func TestFleetRoutingAndNoReplay(t *testing.T) {
	clientTLS, serverTLS := relayTestTLS(t)
	c := testCommander(clientTLS)
	a := &testRelay{clients: []proto.ClientInfo{{ID: "shared01"}, {ID: "abcd0001"}}}
	b := &testRelay{clients: []proto.ClientInfo{{ID: "shared01"}, {ID: "only0002"}, {ID: "abcd0002"}}}
	servers := []ServerConf{startTestRelay(t, serverTLS, "A", a), startTestRelay(t, serverTLS, "B", b)}
	for _, tc := range []struct{ target, wantID, relay string }{
		{"only", "only0002", "B"}, {"only0002", "only0002", "B"}, {"shared", "shared01", "A"},
	} {
		lc, id, err := c.pickTarget(servers, tc.target, "")
		if err != nil {
			t.Fatal(err)
		}
		if id != tc.wantID || lc.name != tc.relay {
			t.Errorf("target %s: got %s via %s", tc.target, id, lc.name)
		}
		_ = lc.conn.CloseWithError(0, "done")
	}
	if lc, _, err := c.pickTarget(servers, "abcd", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		if lc != nil {
			_ = lc.conn.CloseWithError(0, "done")
		}
		t.Fatalf("expected cross-relay ambiguity, got %v", err)
	}
	// OPEN rejection must leave the outcome reported as not_started: the
	// command never reached a client, so this is the one case where a retry
	// cannot duplicate a side effect.
	res, err := c.ExecCapture(servers, "shared01", "default", "test", time.Second)
	if err == nil || !strings.Contains(err.Error(), "test rejection") {
		t.Fatalf("expected OPEN rejection, got %v", err)
	}
	if res == nil || res.State != execNotStarted || !res.SafeToRetry {
		t.Fatalf("rejected OPEN must be reported as not_started/safe_to_retry, got %+v", res)
	}
	if a.opens.Load() != 1 || b.opens.Load() != 0 {
		t.Fatalf("command replayed: A=%d B=%d", a.opens.Load(), b.opens.Load())
	}
}

func TestClientListDistinguishesEmptyPartialAndFailure(t *testing.T) {
	clientTLS, serverTLS := relayTestTLS(t)
	c := testCommander(clientTLS)
	empty := startTestRelay(t, serverTLS, "empty", &testRelay{})
	bad := startTestRelay(t, serverTLS, "bad", &testRelay{listMode: "malformed"})
	clients, failures, err := c.queryClients([]ServerConf{empty}, "")
	if err != nil || clients == nil || len(clients) != 0 || len(failures) != 0 {
		t.Fatalf("empty healthy roster: clients=%v failures=%v err=%v", clients, failures, err)
	}
	clients, failures, err = c.queryClients([]ServerConf{empty, bad}, "")
	if err != nil || len(failures) != 1 || failures[0].Name != "bad" {
		t.Fatalf("partial roster: clients=%v failures=%v err=%v", clients, failures, err)
	}
	_, failures, err = c.queryClients([]ServerConf{bad}, "")
	if err == nil || len(failures) != 1 || !strings.Contains(err.Error(), "no server roster available") {
		t.Fatalf("failed roster must be an error: failures=%v err=%v", failures, err)
	}
	if c.ListClients([]ServerConf{bad}, true) != 3 {
		t.Fatal("all failed list should return exit code 3")
	}
}

func TestHealthRequiresValidStatusAndBoundsHungRPCs(t *testing.T) {
	clientTLS, serverTLS := relayTestTLS(t)
	c := testCommander(clientTLS)
	servers := []ServerConf{
		startTestRelay(t, serverTLS, "healthy", &testRelay{}),
		startTestRelay(t, serverTLS, "malformed", &testRelay{infoMode: "malformed"}),
		startTestRelay(t, serverTLS, "hung-status", &testRelay{infoMode: "hang"}),
		startTestRelay(t, serverTLS, "hung-hello", &testRelay{hangHello: true}),
	}
	start := time.Now()
	rows, reachable := c.probeServers(servers)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("hung handshakes and status RPCs were not bounded: %s", elapsed)
	}
	if reachable != 1 || rows[0].status != "ok" {
		t.Fatalf("only valid status is healthy: reachable=%d rows=%v", reachable, rows)
	}
	for _, row := range rows[1:] {
		if row.status == "ok" {
			t.Fatalf("failed status counted healthy: %v", row)
		}
	}
}

func TestExecTimeoutClosesSessionAndHonorsTransport(t *testing.T) {
	clientTLS, serverTLS := relayTestTLS(t)
	c := testCommander(clientTLS)
	c.cfg.General.Transport = "udp"
	behavior := &testRelay{clients: []proto.ClientInfo{{ID: "long0001"}}, holdExec: true, closeSession: make(chan string, 1)}
	server := startTestRelay(t, serverTLS, "tcp-only", behavior)
	// The session opened, so the command WAS running: a timeout here must not
	// claim the outcome is known, and must not advertise a safe retry.
	res, err := c.execCapture([]ServerConf{server}, "long0001", "default", execOptions{
		Command: "test", ProtoKind: "tcp", Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "exec timed out") {
		t.Fatalf("expected timeout after successful forced TCP OPEN: %v", err)
	}
	if res == nil || res.State != execTimedOut || res.ExitCode != nil || res.SafeToRetry {
		t.Fatalf("timeout must be timed_out with unknown exit and unsafe retry, got %+v", res)
	}
	select {
	case sid := <-behavior.closeSession:
		if sid != "12345678" {
			t.Fatalf("closed wrong session %s", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("exec timeout did not send CLOSE")
	}
}

// The regression that motivated this: a command that printed something and
// then hung used to return only "exec timed out", discarding the bytes it had
// already produced. That made a timeout indistinguishable from "never ran" and
// invited a blind retry of a command that may already have had side effects.
func TestExecTimeoutPreservesPartialOutput(t *testing.T) {
	clientTLS, serverTLS := relayTestTLS(t)
	c := testCommander(clientTLS)
	behavior := &testRelay{
		clients:         []proto.ClientInfo{{ID: "slow0001"}},
		holdExec:        true,
		closeSession:    make(chan string, 1),
		execFirstOutput: "BEFORE_TIMEOUT_MARK\r\n",
	}
	server := startTestRelay(t, serverTLS, "slow", behavior)

	res, err := c.execCapture([]ServerConf{server}, "slow0001", "default", execOptions{
		Command: "test", ProtoKind: "tcp", Timeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "exec timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if res == nil {
		t.Fatal("timeout returned no result, so partial output was discarded")
	}
	if res.State != execTimedOut {
		t.Fatalf("state = %q, want %q", res.State, execTimedOut)
	}
	if res.ExitCode != nil {
		t.Fatalf("exit code must stay unknown on timeout, got %d", *res.ExitCode)
	}
	if res.SafeToRetry {
		t.Fatal("a submitted command that timed out must not be advertised as safe to retry")
	}
	// The bytes captured before the timeout must survive in the raw result.
	if !strings.Contains(res.Output, "BEFORE_TIMEOUT_MARK") {
		t.Fatalf("partial output lost on timeout: %q", res.Output)
	}
	// And in the MCP-facing shape, cleaned and with an honest state.
	structured := execStructured(res, false, 0)
	if got := structured["output"].(string); got != "BEFORE_TIMEOUT_MARK\n" {
		t.Fatalf("sanitized partial output = %q", got)
	}
	if structured["execution_state"] != execTimedOut || structured["safe_to_retry"] != false {
		t.Fatalf("structured timeout report = %+v", structured)
	}
	if structured["exit_code"] != nil {
		t.Fatalf("structured exit_code must be null, got %v", structured["exit_code"])
	}

	// The timeout must still ask the client to stop the process.
	select {
	case sid := <-behavior.closeSession:
		if sid != "12345678" {
			t.Fatalf("closed wrong session %s", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("exec timeout did not send CLOSE")
	}
}

// A data stream that ends without an EXIT frame (relay or client vanished
// while the control connection stayed up) must not wait for timeout_ms.
func TestExecOutputStreamEOFWithoutExitIsLostPromptly(t *testing.T) {
	clientTLS, serverTLS := relayTestTLS(t)
	c := testCommander(clientTLS)
	behavior := &testRelay{
		clients:         []proto.ClientInfo{{ID: "eof00001"}},
		holdExec:        true,
		closeData:       true,
		closeSession:    make(chan string, 1),
		execFirstOutput: "PARTIAL_BEFORE_EOF\r\n",
	}
	server := startTestRelay(t, serverTLS, "eof", behavior)

	// The control connection stays alive, but the data stream ends without an
	// EXIT frame. The attempt must be reported as lost promptly, not after the
	// generous 5s timeout.
	started := time.Now()
	res, err := c.execCapture([]ServerConf{server}, "eof00001", "default", execOptions{
		Command: "test", ProtoKind: "tcp", Timeout: 5 * time.Second})
	if err == nil {
		t.Fatalf("expected an error after stream EOF, got %+v", res)
	}
	if res == nil || res.State != execLost {
		t.Fatalf("state = %+v, want %q", res, execLost)
	}
	if res.ErrorCode != errCodeLost {
		t.Fatalf("error_code = %q, want %q", res.ErrorCode, errCodeLost)
	}
	if res.SafeToRetry {
		t.Fatalf("a submitted command must not be marked safe to retry: %+v", res)
	}
	if !strings.Contains(res.Output, "PARTIAL_BEFORE_EOF") {
		t.Fatalf("output captured before EOF was discarded: %q", res.Output)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("took %s; EOF should abort without waiting for timeout_ms", elapsed)
	}
}

// A connection lost after the command was submitted is also an unknown
// outcome: the process may still be running remotely.
func TestExecLostConnectionIsUnknownOutcome(t *testing.T) {
	clientTLS, serverTLS := relayTestTLS(t)
	c := testCommander(clientTLS)
	behavior := &testRelay{
		clients:         []proto.ClientInfo{{ID: "lost0001"}},
		holdExec:        true,
		closeSession:    make(chan string, 1),
		execFirstOutput: "PARTIAL_BEFORE_LOSS\r\n",
	}
	server := startTestRelay(t, serverTLS, "lost", behavior)

	// A generous exec timeout means the abort must come from the relay going
	// away, not from the local deadline.
	go func() {
		time.Sleep(150 * time.Millisecond)
		behavior.dropConns()
	}()
	res, err := c.execCapture([]ServerConf{server}, "lost0001", "default", execOptions{
		Command: "test", ProtoKind: "tcp", Timeout: 5 * time.Second})
	if err == nil {
		t.Fatalf("expected an error after connection loss, got %+v", res)
	}
	if res == nil || res.State != execLost {
		t.Fatalf("state = %+v, want %q", res, execLost)
	}
	if res.ExitCode != nil || res.SafeToRetry {
		t.Fatalf("lost connection must be an unknown, unsafe-to-retry outcome: %+v", res)
	}
	if !strings.Contains(res.Output, "PARTIAL_BEFORE_LOSS") {
		t.Fatalf("output captured before the loss was discarded: %q", res.Output)
	}
}

func TestExecOutputRetainsBoundedNewestBytes(t *testing.T) {
	var buf execOutputBuffer
	old := strings.Repeat("x", execOutputLimit+17)
	if n, err := buf.Write([]byte(old)); n != len(old) || err != nil {
		t.Fatalf("write failed: n=%d err=%v", n, err)
	}
	newest := "\nlast output must survive\n"
	_, _ = buf.Write([]byte(newest))
	output := buf.String()
	if len(buf.data) > execOutputLimit || !strings.HasSuffix(output, newest) {
		t.Fatal("exec output is unbounded or lost its newest bytes")
	}
	if want := "[output truncated: 43 bytes discarded]\n"; !strings.HasPrefix(output, want) {
		t.Fatalf("expected explicit truncation count, prefix=%q", output[:80])
	}
}
