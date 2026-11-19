# she11

High-performance SSH alternative for humans and AI agents. QUIC-first transport, sessions that survive network drops, and a built-in MCP server — AI drives your Windows/Linux fleet as directly as you do from the CLI.

## Architecture

```text
commander (operator console: CLI + MCP server)
    │  pure outbound; never accepts inbound connections
    ▼
relay / server (authenticated rendezvous plane)
    │  multiple relays are merged into one deduplicated fleet view
    ▼
client (agent on each managed machine: Windows / POSIX)
```

The commander connects to configured relays and drives registered devices through a machine-readable tool surface. UDP (QUIC) is preferred, with TLS/TCP fallback. Sessions are bound to the relay that accepted them; a submitted command is never replayed on another relay after a network failure.

## Capabilities

- **AI-native control**: a built-in MCP server (JSON-RPC 2.0, stdio or loopback HTTP) exposing 7 tools — `check_server`, `list_client`, `exec`, `start_shell`, `shell_write`, `shell_read`, `shell_close`. All results carry `structuredContent`.
- **One-shot exec**: stateless command execution in a fresh process, with real stdout/stderr separation (`exec_pipe`), optional `cwd`/`env`, forced PTY, output budgets, and explicit retry semantics.
- **Persistent interactive sessions**: PTY shells with incremental reads, raw byte input, Ctrl-C, and shell-integration completion markers.
- **Human CLI**: relay health probe, merged fleet roster, one-shot exec, and interactive shells with aligned pipe-separated tables.
- **Cross-platform**: Windows and POSIX clients; `pwsh` / `powershell` / `cmd` / `fish` / `zsh` / `bash` / `sh` / `default`.
- **Honest failure model**: tool errors, execution state, and process exit codes are reported independently. Only `not_started` is safe to retry.

Full tool reference, error vocabulary, and verified integration notes: [`doc/she11_commander_skill.md`](doc/she11_commander_skill.md).

## Build

The build script injects deployment values (pinned server certificate, fleet magic, relay endpoints) into `src/shared/gen/config_gen.go` at compile time.

```powershell
# 1. Generate your own pinned keypair (never reuse the committed test keys)
python builder.py gencert --out config/server.crt --key config/server.key

# 2. Build commander (Windows / amd64)
python builder.py commander --target windows -a amd64 --server-cert config/server.crt -o bin/commander.exe

# client and server are built the same way
python builder.py client --target linux -a amd64 --server-cert config/server.crt --magic "0000000000000000" --udp-server-list "relay.example.com:2077" --ip-protocol prefer-ipv4 -o bin/client
python builder.py server --target linux -a amd64 -o bin/server
```

Run `python builder.py --help` for all roles and options.

For a plain `go build ./...` without the build script, copy `src/shared/gen/config_gen.go.example` to `src/shared/gen/config_gen.go` and replace its placeholder values. The generated file is gitignored — it carries production endpoints and must never be committed.

## Quick start

1. Edit `config/she11_server.toml` (listen planes, `magic_list`, `commander_password`) and start the relay.
2. Edit `config/she11_commander.toml` with your relay's commander-plane address and password, then place it next to the commander binary (or pass `-c`).
3. Verify the fleet:

```powershell
./commander.exe --check-server
./commander.exe --list-client
```

4. For AI control, start the MCP server:

```powershell
./commander.exe -c she11_commander.toml --mcp stdio
```

## Security notes

- `config/server.key` (production private key), live/deployed TOML configs, `bin/`, and the generated `config_gen.go` are gitignored. Keep them off version control.
- `config/server_test.crt` / `config/server_test.key` are a freshly generated, self-signed **test-only** Ed25519 keypair shipped for local development and examples. They are not used by any real deployment and grant no access to a production fleet.
- Relay configs in `config/` are templates with placeholder addresses and `CHANGE-ME` passwords. Replace them before deploying.
- MCP HTTP is plain HTTP: bind to loopback or front it with TLS; never expose it directly to the internet.

## Tests

```powershell
go test ./src/...
```

## License

MIT — see [LICENSE](LICENSE).
