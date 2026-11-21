#!/usr/bin/env bash
# Install or remove a she11 relay (she11_server) as a systemd service.
#
# Install mode (default) replaces any existing service with the same name:
# stop, disable and delete the old unit, then write and start the new one.
# Remove mode only deletes the service; the binary and config are untouched.
set -euo pipefail

SERVICE_NAME="she11-server"
BINARY=""
CONFIG=""
RUN_USER="root"
REMOVE=0

usage() {
    cat <<'EOF'
Usage:
  install_service.sh -f BINARY -c CONFIG [options]
  install_service.sh --remove [options]

Install a she11 relay (she11_server) as a systemd service, or remove an
existing one.

Install mode (default):
  -f, --file PATH      Absolute path to the she11_server executable. Required.
  -c, --config PATH    Absolute path to the server config (she11_server.toml).
                       Required.
  -n, --name NAME      systemd service name (without the .service suffix).
                       Default: she11-server. Valid characters: [A-Za-z0-9_.-].
  -u, --user USER      User the service runs as. Default: root.
  -h, --help           Show this help.

Remove mode:
  --remove             Disable, stop and delete the service unit. The binary
                       and config files are left untouched.
  -n, --name NAME      Service name to remove. Default: she11-server.

Install replaces an existing service with the same name.

Examples:
  sudo install_service.sh -f /opt/she11/she11_server -c /opt/she11/she11_server.toml
  sudo install_service.sh -f /opt/she11/she11_server -c /opt/she11/she11_server.toml -u nobody
  sudo install_service.sh --remove
  sudo install_service.sh --remove -n my-relay
EOF
}

# --- argument parsing ---
OPTS=$(getopt -o hf:c:n:u: -l help,file:,config:,name:,user:,remove -- "$@") \
    || { usage >&2; exit 1; }
eval set -- "$OPTS"
while true; do
    case "$1" in
        -h|--help)      usage; exit 0 ;;
        -f|--file)      BINARY="$2";        shift 2 ;;
        -c|--config)    CONFIG="$2";        shift 2 ;;
        -n|--name)      SERVICE_NAME="$2";  shift 2 ;;
        -u|--user)      RUN_USER="$2";      shift 2 ;;
        --remove)       REMOVE=1;           shift ;;
        --)             shift; break ;;
        *)              usage >&2; exit 1 ;;
    esac
done
[[ $# -eq 0 ]] || { echo "error: unexpected argument: $1" >&2; exit 1; }

# --- service name: strip a trailing .service, then validate ---
SERVICE_NAME="${SERVICE_NAME%.service}"
if [[ ! "$SERVICE_NAME" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]]; then
    echo "error: invalid service name: $SERVICE_NAME" >&2
    echo "       allowed: [A-Za-z0-9_.-], must not start with a dot or dash" >&2
    exit 1
fi

# --- root check: writing a unit and controlling systemd requires root ---
if [[ "$(id -u)" -ne 0 ]]; then
    echo "error: this script must run as root (try: sudo $0 ...)" >&2
    exit 1
fi

UNIT="/etc/systemd/system/${SERVICE_NAME}.service"

# --- remove mode: delete the service, leave the binary and config alone ---
if [[ "$REMOVE" -eq 1 ]]; then
    if [[ -f "$UNIT" ]]; then
        systemctl stop "$SERVICE_NAME.service" 2>/dev/null || true
        systemctl disable "$SERVICE_NAME.service" 2>/dev/null || true
        rm -f "$UNIT"
        rm -rf "/etc/systemd/system/${SERVICE_NAME}.service.d"
        systemctl daemon-reload
        echo "removed service: $SERVICE_NAME"
    else
        echo "service not found: $SERVICE_NAME (nothing to remove)"
    fi
    exit 0
fi

# --- install mode: validate required arguments ---
[[ -n "$BINARY" ]] || { echo "error: -f/--file is required" >&2; exit 1; }
[[ -n "$CONFIG" ]] || { echo "error: -c/--config is required" >&2; exit 1; }
[[ "$BINARY" == /* ]] || { echo "error: -f must be an absolute path: $BINARY" >&2; exit 1; }
[[ "$CONFIG" == /* ]] || { echo "error: -c must be an absolute path: $CONFIG" >&2; exit 1; }
[[ -x "$BINARY" ]] || { echo "error: binary not found or not executable: $BINARY" >&2; exit 1; }
[[ -f "$CONFIG" ]] || { echo "error: config not found: $CONFIG" >&2; exit 1; }
if ! id "$RUN_USER" >/dev/null 2>&1; then
    echo "error: user does not exist: $RUN_USER" >&2
    exit 1
fi

WORKDIR="$(dirname "$BINARY")"

# --- replace any existing service with the same name ---
if [[ -f "$UNIT" ]]; then
    echo "replacing existing service: $SERVICE_NAME"
    systemctl stop "$SERVICE_NAME.service" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME.service" 2>/dev/null || true
    rm -f "$UNIT"
    rm -rf "/etc/systemd/system/${SERVICE_NAME}.service.d"
fi

# --- write the unit ---
cat > "$UNIT" <<EOF
[Unit]
Description=she11 relay server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
ExecStart="$BINARY" -c "$CONFIG"
WorkingDirectory=$WORKDIR
Restart=always
RestartSec=5
LimitNOFILE=65535
StandardOutput=journal
StandardError=journal
SyslogIdentifier=$SERVICE_NAME

[Install]
WantedBy=multi-user.target
EOF

# --- load and start ---
systemctl daemon-reload
systemctl enable "$SERVICE_NAME.service" >/dev/null
systemctl start "$SERVICE_NAME.service"

echo
echo "service installed: $SERVICE_NAME"
systemctl status --no-pager -l "$SERVICE_NAME.service" | head -15 || true
