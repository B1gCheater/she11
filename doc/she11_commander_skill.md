---
name: she11_commander_skill
description: Drive the she11 remote-management network through its `she11_commander` binary (MCP server + CLI). Use when the session exposes a she11_commander binary or a she11_commander.toml config and the task requires remote command execution, fleet inventory, relay health probing, or interactive shells on registered Windows/POSIX machines — e.g. "run a command on my VM", "list online clients", "check relay status", "open a remote shell", "edit or delete files through a remote shell". Covers stdio and HTTP transports, one-shot exec (pipe and PTY backends), persistent interactive PTY sessions, target resolution, retry semantics, error codes, and verified integration gotchas.
---

# she11 Commander Skill

she11 is a cross-platform, SSH-alternative remote-management network with high-performance UDP (QUIC) transport and TCP fallback, built so sessions survive hostile networks. Three roles:

- **commander** — the operator console you drive (Windows/POSIX binary). A pure outbound client; it never accepts inbound connections. Exposes a CLI and an MCP server.
- **relay (server)** — the rendezvous plane. The commander connects to its *commander plane* port; clients register on their own plane. Multiple relays are merged into one deduplicated fleet view.
- **client** — an agent on each managed machine (Windows or POSIX). Registered devices appear in the roster with their hostname, OS/arch, username, available shells, and capability flags.

The commander connects to configured relays and drives devices through **7 MCP tools** (fully machine-readable via `structuredContent`) plus a CLI for humans. This file is written for an AI agent: no pleasantries, only executable facts. Where document and code disagree, the code wins (`src/commander/mcp.go`, `src/commander/interactive.go`, `src/shared/proto/messages.go`); report the drift to the maintainer.

## 1. Five rules to internalize first

1. **Read three axes independently**: whether the tool call itself succeeded (`isError`), whether the command ran to completion (`execution_state`), and the process exit code (`exit_code`). They are orthogonal. `completed` + `exit_code=7` means "the command finished and returned 7" (e.g. `grep` with no match) — a successful call for a failing command, not a tool fault.
2. **Only `execution_state=not_started` is safe to retry.** `timed_out` / `lost` mean the command may already have executed; retrying can duplicate side effects.
3. **Every `exec` is a fresh process.** cwd, environment and shell variables do not survive across calls. Keep state with `start_shell`.
4. **Silence ≠ completion.** A `shell_read` returning means only "output went quiet". `command_done` is true only when a complete OSC `133;D` sequence appears in the raw stream; without shell integration it is always false.
5. **Take machine-readable data from `structuredContent` only.** `content[].text` is a compatibility layer for legacy callers and humans; do not parse it for decisions.

## 2. Prerequisites and configuration

Required: a `she11_commander` binary and a TOML config with at least one `[[server]]` entry. Relay `addr` points at the relay's **commander plane** (not the client plane); `password` must equal the relay's `auth.commander_password`.

```toml
[general]
shell_type = "default"   # exec/start_shell default when caller omits shell_type
transport  = "auto"      # auto | udp | tcp
timeout    = 10          # per-step seconds: dial, handshake, roster, OPEN, close

[[server]]               # multiple entries allowed, tried in config order
name     = "relay-1"
addr     = "relay.example.com:2079"
password = "<commander password>"

[mcp]
listen = "127.0.0.1:8899"   # http mode only
token  = "<bearer token>"   # http mode only; empty = no auth
```

Known facts:

- With zero `[[server]]` entries, MCP startup fails immediately (exit 2).
- The `[mcp] mode` field is **ignored**: MCP mode must be started with the `--mcp` flag.
- Sessions live in the commander process memory only; there is no session recovery. A restarted commander loses every in-memory session.

## 3. Starting the MCP server

### 3.1 stdio (recommended for a local AI)

```text
she11_commander -c she11_commander.toml --mcp stdio
```

- One JSON-RPC 2.0 message per line on stdin; stdout carries **only** protocol messages; diagnostics go to stderr.
- Single-line cap 1 MiB; an oversized or unreadable line is reported on stderr and ends the read loop.
- No authentication — whoever can start the process can use it.
- The process exits on stdin EOF. Keep one long-lived process for `start_shell` flows.

### 3.2 HTTP (loopback or behind a TLS tunnel)

```text
she11_commander -c she11_commander.toml --mcp http --mcp-listen 127.0.0.1:8899 --mcp-token <token>
```

- Endpoint: `POST http://<listen>/mcp`, body = a single JSON-RPC message (no batches, no SSE session).
- `Authorization: Bearer <token>` is required when a token is configured (missing/wrong → 401).
- `MCP-Protocol-Version: 2025-06-18` is optional; a different value → 400.
- `Content-Type` is optional; if present its media type must be `application/json` (charset allowed), else 415.
- `Origin`, when present, must be a loopback host (`localhost`, any `127.x.x.x`, `::1`) without path/query/fragment/userinfo, else 403. The port is not checked.
- Plain HTTP: bind loopback only or front it with TLS. Never expose it to the public internet.

### 3.3 HTTP surface

| Condition | HTTP status | JSON-RPC code |
|---|---|---|
| Well-formed request | 200 + result | — |
| Notification (no `id`) | 202, no body | — |
| Malformed JSON body | 400 | `-32700` |
| Non-JSON-RPC 2.0 body | 400 | `-32600` |
| Bad tool-call params | 200 | `-32602` (protocol error — fix the call, do not retry it) |
| Missing/wrong Bearer token | 401 | `-32001` |
| Disallowed `Origin` | 403 | `-32000` |
| Wrong `MCP-Protocol-Version` | 400 | `-32600` |
| `Content-Type` not `application/json` | 415 | `-32600` |
| Non-POST method | 405 | `-32600` |
| Body over 1 MiB | 413 | `-32600` |

CORS: preflight `OPTIONS` → 204; allowed headers `Content-Type, Authorization, MCP-Protocol-Version`; methods `POST, OPTIONS`.

## 4. Handshake and self-check (run in this order)

1. `initialize` → expect `protocolVersion=2025-06-18`, `serverInfo.name=she11_commander`.
2. `tools/list` → exactly 7 tools; `exec`'s input schema includes `cwd`/`env`/`tty` and its output schema includes `error_code`/`exec_backend`/`stdout`/`stderr`.
3. `check_server` → `reachable >= 1` (means *authenticated* status RPC success, not merely TCP-open).
4. `list_client` → target device visible; note its `caps` (`exec_pipe` present = modern client; empty caps = old client, exec falls back to PTY).
5. `exec` a marker command → `execution_state=completed`, `exit_code=0`, marker in `output`.
6. `exec` the same command with `tty:true` → `exec_backend=pty`, `stdout`/`stderr` null.
7. Interactive: `start_shell` → `shell_write("echo OK\r")` → `shell_read` shows `OK` → `shell_close`.

`ping` (`{"jsonrpc":"2.0","id":9,"method":"ping"}` → empty result) is a local liveness check; it never touches the relays.

If any step behaves differently, check the binary/build version before changing caller logic.

## 5. Tool roster

| Tool | Purpose | Side effects |
|---|---|---|
| `check_server` | Per-relay authenticated health probe | read-only |
| `list_client` | Merged, deduplicated online-device roster | read-only |
| `exec` | One-shot command in a fresh process | creates a process on the target |
| `start_shell` | Open a persistent interactive PTY session | consumes one session slot |
| `shell_write` | Write **raw bytes** to a session (no auto-Enter) | may trigger command execution |
| `shell_read` | Incremental read of new output (until quiet) | consumes buffer |
| `shell_close` | Close session and terminate process tree | ends process |

Calls use `{"jsonrpc":"2.0","id":N,"method":"tools/call","params":{"name":"<tool>","arguments":{...}}}`.

Parameter validation failure is a **protocol error** (JSON-RPC `-32602`), not an `isError=true` tool result — it produces no result at all. Fix the arguments; never retry the same request. Always include an `id`: a request without one is a notification — it still executes, but the response is discarded (stdio: nothing; HTTP: 202).

## 6. Target resolution (`client_id`)

1. Exact match on the full 8-char lowercase base32 id wins.
2. Otherwise a **prefix unique across the whole merged fleet**, or an exact `observed_addr` or `hostname`.
3. Multiple hits → `target_ambiguous` (never guessed); zero hits → `target_not_found`.

The roster merges and dedupes across all configured relays; one device may be seen by several (`sightings`/`relays`). A session pins to the relay that accepted it; a submitted command is **never replayed** on another relay after a network problem.

## 7. Tool reference

Every tool result (success or tool-level failure) carries `structuredContent` plus a `content[].text` compatibility layer. `error_code` is the stable machine code; `error` is human text — never parse `error` for decisions.

### 7.1 `check_server` — relay health

No arguments. Authenticated status RPC per configured relay.

| Field | Type | Meaning |
|---|---|---|
| `servers[]` | array | One row per configured relay |
| `servers[].name` / `addr` / `status` | string | `status="ok"` = authenticated status RPC succeeded |
| `servers[].clients` | int | Registered *connections* on that relay — can exceed the deduplicated device count |
| `servers[].uptime_sec` | int | Relay uptime |
| `reachable` / `total` | int | Reachable count / configured count |
| `error_code` | string | `relay_unreachable` when all relays are unreachable, else empty |

`isError=true` if and only if `reachable=0`.

### 7.2 `list_client` — fleet roster

No arguments. Read-only; partial relay failures are reported alongside rows instead of failing the call.

| Field | Meaning |
|---|---|
| `clients[]` | Deduplicated device rows |
| `clients[].id` | 8-char device id |
| `clients[].hostname` / `os` / `arch` / `username` / `via` | Metadata; `via` = identity name of the relay that reported the row (not the transport; all relays in `relays[]`) |
| `clients[].shells[]` | Available shells on that device, best first |
| `clients[].caps[]` | `exec_pipe`, `exec_cwd_env`, `exec_split_stderr`. Empty = old client; absence always means "not supported", never "assume yes" |
| `clients[].sightings` / `relays[]` | How many relays see it / which ones |
| `clients[].active_sessions` / `max_shell` | Current sessions / client-side cap (`0` = unlimited) |
| `total` | Device count |
| `server_errors[]` | Present only when some relays failed (`name`/`addr`/`error`). An empty roster *with* errors is NOT proof that no device is online |
| `error_code` | Only on total failure (`relay_unreachable` / `no_relay_configured`); then `isError=true`, empty roster, no `server_errors` |

### 7.3 `exec` — one-shot command

| Param | Required | Default | Meaning |
|---|---|---|---|
| `client_id` | yes | — | Full id or globally unique prefix |
| `command` | yes | — | Interpreted by the remote shell; no extra escaping is added — use the target platform's syntax |
| `shell_type` | no | config value | `pwsh` / `powershell` / `cmd` / `fish` / `zsh` / `bash` / `sh` / `default` |
| `timeout_ms` | no | 30000 | Wait for process exit *after* the session opens; not a total deadline |
| `cwd` | no | empty | Working dir on target; must exist. Requires the non-PTY backend |
| `env` | no | `{}` | String key/values merged into the inherited environment (PATH preserved). Requires the non-PTY backend |
| `tty` | no | false | `true` forces the legacy PTY backend (merges stdout/stderr) |
| `max_output_bytes` | no | 0 | Response budget; head+tail kept, omitted middle counted in `discarded_bytes` |
| `raw_output` | no | false | `true` returns the exact byte stream, no control-sequence sanitization |

**Backend selection.** Default is `exec_pipe`: non-PTY, real stdout/stderr separation, no ConPTY, stdin at EOF from the start — used only when the client advertises `exec_pipe` *and* the relay forwards it, otherwise the call falls back to PTY. `tty=true` forces PTY (programs that require a console). Requesting `cwd`/`env` on a link that cannot support them fails with `error_code=capability_unsupported`, `execution_state=not_started`, `safe_to_retry=true` — the parameters are never silently dropped.

**Output (`structuredContent`).**

| Field | Type | Meaning |
|---|---|---|
| `execution_state` | enum | `not_started` / `completed` / `timed_out` / `lost` |
| `exit_code` | int\|null | null when unknown (timeout, disconnect) |
| `shell` / `client_id` | string | Requested shell (may be `default`) / resolved device id |
| `exec_backend` | `"pipe"` \| `"pty"` \| `""` | Empty = nothing was submitted |
| `output` | string | Merged arrival-order view (compat field) |
| `stdout` / `stderr` | string\|null | Pipe backend only; null under PTY (the OS merged them — never fabricated) |
| `duration_ms` | int | Total attempt time incl. target resolution and session setup |
| `truncated` | bool | Cut by the internal 1 MiB cap or the response budget |
| `discarded_bytes` | int | Bytes missing from `output` |
| `output_raw` | bool | Raw bytes returned? |
| `error_code` | string | Stable failure class; empty on success |
| `error` | string | Human-readable detail; do not parse |
| `safe_to_retry` | bool | True only when `not_started` |

**Outcome judgment and retry.**

| `execution_state` | `exit_code` | Meaning | Action |
|---|---|---|---|
| `not_started` | null | Never reached a client | Fix params, retry |
| `completed` | known | Ran to completion | Judge by exit code |
| `timed_out` | null | Submitted, timed out and killed locally | Outcome unknown — verify target state before rerunning |
| `lost` | null | Submitted, connection lost | Same as above |

A `completed` result can still be `isError=true` with `error_code=output_drain_timeout`: the exit code is known but the output drain timed out, so captured output may be incomplete.

**Output sanitizing and budgets.** Default output is sanitized (CSI/OSC/DCS escape sequences stripped, `\r\n` → `\n`). The internal buffer keeps the newest 1 MiB; `max_output_bytes` is an additional response budget — the two are independent. When truncated, `truncated=true` and `discarded_bytes` reports the missing volume.

The `content[].text` compat format is `exit=N\n<output>` on success, `exec failed: ...` (+ `partial output:`) on failure.

### 7.4 `start_shell` — open a session

| Param | Required | Meaning |
|---|---|---|
| `client_id` | yes | Target device |
| `shell_type` | no | Omit = commander config default; `default` = client picks its best |

Output: `session_id` (required field; empty string on failure), `client_id`, `shell`, plus `error_code`/`error` on failure. Failure codes include `target_not_found`, `target_ambiguous`, `relay_unreachable`, `client_offline`, `shell_unavailable`, `session_limit`, `open_timeout`.

The session occupies one of the client's `max_shell` slots until `shell_close` or natural exit.

### 7.5 `shell_write` — raw input

| Param | Required | Meaning |
|---|---|---|
| `session_id` | yes | From `start_shell` |
| `input` | yes | **Raw bytes**; may be empty (probe); never trimmed; nothing is appended |

- To run a command: send the command text, then a separate `"\r"` (Enter is `\r` on both Windows and POSIX terminals).
- Interrupt a running command: send `"\x03"` (Ctrl-C).
- A write with no line terminator returns a `note` that it will not execute.

Output: `ok` (bool), `bytes` (accepted count), `note`, `error_code` (`session_not_found` / `connection_lost`).

### 7.6 `shell_read` — incremental output

| Param | Default | Meaning |
|---|---|---|
| `session_id` | required | Session |
| `timeout_ms` | 8000 | Max wait |
| `quiet_ms` | 300 | Silence window that ends the read; raise it for bursty output |
| `max_output_bytes` | 0 | Response budget |
| `raw_output` | false | Exact terminal byte stream incl. escape sequences |

Output: `output`, `session_exited`, `exit_code` (structured `null` when unknown; the text layer prints `-1`), `truncated`, `discarded_bytes`, `output_raw`, `command_done`, plus `error_code` on failure.

Semantics:

- **Incremental**: each call returns only bytes produced since the previous read.
- Silence ≠ completion: a program pausing longer than `quiet_ms` is indistinguishable from a finished one.
- `command_done=true` only when a complete OSC `133;D` (BEL or `ESC \` terminated) appears in the raw stream; without shell integration it stays false — a known boundary.
- Unread-output cap: newest 1 MiB; dropped bytes land in `discarded_bytes`.
- After natural session exit, the last output stays readable for up to 5 minutes.

### 7.7 `shell_close` — teardown

Closes the session and terminates its process tree on the client; waits for the remote teardown ack (the freed slot is immediately reusable), and on ack timeout reports an error but cleans up locally anyway.

Output: `closed`, `session_id`, `exit_code` (null when unknown), `error_code` (`session_not_found` / `open_timeout` / `connection_lost`).

Observed: closing an active shell can return `closed=true` with a non-zero `exit_code` (forced termination). Judge success by `closed`.

## 8. Standard workflows

### One-shot exec

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{
  "client_id":"k7jfe5qf","command":"echo MCP_SELFCHECK","shell_type":"default","timeout_ms":30000}}}
```

### exec with cwd and env (pipe backend only)

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"exec","arguments":{
  "client_id":"k7jfe5qf","command":"echo %BUILD_ID%","shell_type":"cmd",
  "cwd":"<remote-work-dir>","env":{"BUILD_ID":"42"},"timeout_ms":15000}}}
```

### Interactive session (recommended read loop)

```text
start_shell(client_id, shell_type?)  -> session_id
shell_write(session_id, "command\r")          # Enter must be sent explicitly, as "\r"
loop:
    r = shell_read(session_id, timeout_ms=2000, quiet_ms=300)
    append r.output
    if r.session_exited: stop
    if r.command_done: stop                    # only with shell integration enabled
    if your_time_budget_exceeded: stop         # never treat silence as completion
shell_close(session_id)
```

Interrupt a running command with `\x03` via `shell_write`.

## 9. `error_code` vocabulary

| `error_code` | Meaning | Typical handling |
|---|---|---|
| `target_not_found` | No match for id/prefix/hostname | Re-run `list_client` |
| `target_ambiguous` | Prefix matches multiple devices | Use the full 8-char id |
| `no_relay_configured` | Commander has no relays | Fix config |
| `relay_unreachable` | All configured relays unreachable | Check network/relay state |
| `client_offline` | Device not online | Wait for reconnect |
| `shell_unavailable` | Target lacks that shell | Use a value from `list_client.shells` |
| `session_limit` | Client `max-shell` full | `shell_close` to free a slot |
| `open_timeout` | OPEN → client ack timed out | Treated as not submitted (`safe_to_retry=true`); if the ack was lost the client may still have spawned — verify non-idempotent commands before rerunning |
| `auth_denied` | Password/cert/magic mismatch | Fix credentials or rebuild |
| `protocol_mismatch` | Mode or protocol incompatible | Upgrade one side |
| `client_rejected` | Client rejected the session | Read `error` text |
| `capability_unsupported` | Link cannot do `cwd`/`env` etc. | Drop the param or upgrade both ends |
| `invalid_argument` | Bad argument (e.g. cwd missing) | Fix the argument |
| `exec_timeout` | Timed out after submission | Outcome unknown; do not blindly rerun |
| `connection_lost` | Connection lost after submission | Same as above |
| `output_drain_timeout` | Process exited but output drain timed out | Captured output still usable |
| `session_not_found` | Session id unknown/expired | `start_shell` again |
| `internal` | Unclassified | Read `error` text |

## 10. Limits and boundaries (honest list)

- No `list_sessions`, no session recovery: a commander restart drops every in-memory session.
- No background-task API: `exec` `timeout_ms` expiry means the caller gave up.
- An HTTP disconnect/cancel does not propagate to remote execution; rely on timeouts or explicit `shell_close`.
- PTY merges streams by OS design: `stdout`/`stderr` are null there.
- Per-device concurrency is capped by the client's `--max-shell` (`0` = unlimited); the commander cannot bypass it.
- No `audit.jsonl` implementation; logs are relay connection logs and shell dumps only.
- MCP HTTP is single POST + JSON response; no SSE, no session negotiation headers.
- Tool set is fixed at 7; no `shell_resize` — MCP sessions are fixed at 200x50.
- `tools/call` must carry an `id`; without one it is a notification (executes, response discarded).
- Internal output buffer keeps the newest 1 MiB; `max_output_bytes` is a separate response budget.

## 11. Verified integration notes (live deployment)

- `exec` default backend = pipe with separated stdout/stderr when the client advertises `exec_pipe`; `tty=true` yields `exec_backend=pty` and null channel fields.
- `cwd` + `env` work end-to-end through a modern client (observed env override with `echo %VAR%`).
- `check_server` counted 2 connections for a fleet whose deduplicated roster had 1 device — server-side counts are connections, not devices.
- A missing `cwd` returns `invalid_argument`, `execution_state=not_started`, `safe_to_retry=true`.
- A `tools/call` without `id` executes but the response is discarded; a UTF-8 BOM before the first `{` yields `-32700 parse error` on line 1 — write request files BOM-less.
- Batch-style "start process → feed lines → close stdin" runs in order and the commander exits on EOF; sessions do not survive the process (`session_not_found`).
- `shell_close` on an active session returns `closed=true` even when `exit_code` is non-zero (forced termination).

## 12. CLI quick reference

The same binary is a human/ops CLI. Config lookup: `./she11_commander.toml` in the working directory unless `-c PATH` says otherwise. `-j`/`--json` switches probe/list output to machine-readable JSON matching the MCP field names.

```text
she11_commander --check-server [PASS:ADDR,...]        probe relay health
she11_commander --list-client [PASS:ADDR,...]         list clients (merged view)
she11_commander --start-shell TARGET [--shell-type TYPE] [--server PASS:ADDR,...] [--tcp|--udp]
she11_commander --exec TARGET -- CMD                   one-shot command
she11_commander --mcp {stdio|http} [--mcp-listen ADDR] [--mcp-token TOKEN]
```

- Targets accept the same resolution rules as MCP `client_id` (full id, unique prefix, observed address, hostname).
- Text output for `--list-client` and `--check-server` is a pipe-separated table (dynamic widths, CJK-aware alignment); relay failures print as `server error: name (addr): reason` lines below the table, and `--check-server` ends with `N of N reachable`.
- Exit codes: `0` success; `2` usage/config error; `3` no relay reachable (probe/list); `9` single-instance mutex held by another commander.

## 13. Build and version note

Build from source with the bundled builder (server certificate is the only build-time input for the commander; it is embedded as the trust anchor):

```text
python builder.py commander --target windows -a amd64 --server-cert config/server.crt -o bin/commander.exe
```

Roles: `client`, `server`, `commander` (each takes `--target`/`-a`/`-o`; client builds additionally embed fleet magic and relay addresses via genconfig).

This document targets the 2026-10-24 commander build (7 tools + `structuredContent` + `exec_pipe`), with the 2026-11-19 CLI table output for `--check-server`/`--list-client`. If a newer binary behaves differently, re-check `tools/list` schemas and the project source — the source is authoritative over this document.
