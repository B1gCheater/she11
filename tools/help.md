# install_service.sh

Install or remove a she11 relay (`she11_server`) as a systemd service on Linux.

## Requirements

- A Linux host with systemd.
- The `she11_server` binary (Linux build) and its `she11_server.toml` config,
  both reachable by absolute path.
- Root privileges: run the script with `sudo`.

## Install

```bash
sudo install_service.sh -f /opt/she11/she11_server -c /opt/she11/she11_server.toml
```

With a custom service name and a non-root run user:

```bash
sudo install_service.sh \
    -f /opt/she11/she11_server \
    -c /opt/she11/she11_server.toml \
    -n my-relay \
    -u nobody
```

Install replaces any existing service with the same name: it stops, disables
and deletes the old unit, then writes and starts the new one. Re-running the
same command is therefore safe and idempotent.

## Options

| Option | Required | Default | Meaning |
|---|---|---|---|
| `-f, --file PATH` | install | — | Absolute path to the `she11_server` executable. |
| `-c, --config PATH` | install | — | Absolute path to the server config. |
| `-n, --name NAME` | no | `she11-server` | systemd service name, without the `.service` suffix. Valid characters: `[A-Za-z0-9_.-]`. |
| `-u, --user USER` | no | `root` | User the service runs as. |
| `--remove` | remove | — | Remove the service instead of installing. |
| `-h, --help` | no | — | Print usage. |

## Remove

Removes the service only — the binary and config are left untouched.

```bash
sudo install_service.sh --remove
sudo install_service.sh --remove -n my-relay
```

## Generated unit

The script writes `/etc/systemd/system/<name>.service`:

- `ExecStart="<binary>" -c "<config>"`
- `WorkingDirectory` set to the binary's directory (so relative cert paths
  in the config resolve correctly)
- `Restart=always`, `RestartSec=5`
- `SyslogIdentifier=<name>` — logs appear under `journalctl -u <name>`

## Notes

- The binary must be executable and both paths must be absolute; the script
  validates this before touching systemd.
- Non-root users cannot bind ports below 1024. Use `root`, grant
  `CAP_NET_BIND_SERVICE`, or bind a high port instead.
- The she11 server applies its own single-instance mutex when built with
  `--mutex`; this script does not add process-level locking of its own.
