package server

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const usage = `she11_server — public relay: client registry + auth + byte splicing
               never dials out, never runs commands

usage:
  she11_server [-c CONFIG] [options]
  she11_server -h

config:
  -c, --config PATH          toml config path. If given, ALL other options are
                             ignored. (default lookup: ./she11_server.toml)
listen — client plane:
  --tcp-client-listen LIST   "0.0.0.0:1337,6666,127.0.0.1:888"  comma list
  --udp-client-listen LIST   "[::]:6666,2077"  (QUIC)
  bare port = dual-stack all NICs; port REQUIRED
listen — commander plane:
  --tcp-commander-listen LIST   same rules
  --udp-commander-listen LIST   same rules
tls (both REQUIRED, no plaintext mode):
  --cert PATH                server certificate file
  --key  PATH                server private key file
auth:
  --magic-list LIST          accepted fleet magics "m1,m2" (REQUIRED)
  --commander-password STR   operator password (REQUIRED)
  --max-wrong "N,SEC"        wrong attempts,ban seconds (default 3,60)
limits:
  --max-commander N          concurrent commanders (default: unlimited)
  --max-clients N            registered clients (default: 2000)
  --max-sessions-total N     relay sessions (default: 0 = unlimited)
  --open-shell-timeout SEC   OPEN -> ACCEPT wait (default: 5)
logging:
  --log-folder DIR           shell_dump/ + connection/ (default: none)
  --log-level LVL            info|debug|warn|error (default: info)
identity:
  --server-name NAME
  --public-addr ADDR         what commanders should dial
misc:
  -h, --help
`

// ParseCLI processes args; returns (nil, nil) after printing help.
func ParseCLI(args []string) (*Config, error) {
	fs := flag.NewFlagSet("she11_server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		configPath string
		tcpCL      string
		udpCL      string
		tcpCmdL    string
		udpCmdL    string
		cert, key  string
		magicList  string
		password   string
		maxWrongS  string
		maxCmdr    int
		maxClients int
		maxSessTot int
		openTO     int
		logFolder  string
		logLevel   string
		srvName    string
		pubAddr    string
		help       bool
	)
	fs.StringVar(&configPath, "c", "", "")
	fs.StringVar(&configPath, "config", "", "")
	fs.StringVar(&tcpCL, "tcp-client-listen", "", "")
	fs.StringVar(&udpCL, "udp-client-listen", "", "")
	fs.StringVar(&tcpCmdL, "tcp-commander-listen", "", "")
	fs.StringVar(&udpCmdL, "udp-commander-listen", "", "")
	fs.StringVar(&cert, "cert", "", "")
	fs.StringVar(&key, "key", "", "")
	fs.StringVar(&magicList, "magic-list", "", "")
	fs.StringVar(&password, "commander-password", "", "")
	fs.StringVar(&maxWrongS, "max-wrong", "", "")
	fs.IntVar(&maxCmdr, "max-commander", 0, "")
	fs.IntVar(&maxClients, "max-clients", 0, "")
	fs.IntVar(&maxSessTot, "max-sessions-total", 0, "")
	fs.IntVar(&openTO, "open-shell-timeout", 0, "")
	fs.StringVar(&logFolder, "log-folder", "", "")
	fs.StringVar(&logLevel, "log-level", "", "")
	fs.StringVar(&srvName, "server-name", "", "")
	fs.StringVar(&pubAddr, "public-addr", "", "")
	fs.BoolVar(&help, "h", false, "")
	fs.BoolVar(&help, "help", false, "")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if help || (len(args) > 0 && args[0] == "--help") {
		fmt.Print(usage)
		return nil, nil
	}

	// Config file mode.
	if configPath != "" {
		cfg, err := LoadTOML(configPath)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}
	// Portable default: ./she11_server.toml if present.
	if _, err := os.Stat("she11_server.toml"); err == nil && fs.NFlag() == 0 {
		cfg, err := LoadTOML("she11_server.toml")
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	// Pure parameter mode.
	var cfg Config
	cfg.Identity.Name = srvName
	cfg.Identity.PublicAddr = pubAddr
	cfg.TLS.Cert = cert
	cfg.TLS.Key = key
	cfg.Auth.MagicList = SplitList(magicList)
	cfg.Auth.CommanderPassword = password
	cfg.Limits.MaxCommander = maxCmdr
	cfg.Limits.MaxClients = maxClients
	cfg.Limits.MaxSessionsTotal = maxSessTot
	cfg.Limits.OpenShellTimeout = openTO
	cfg.Log.Folder = logFolder
	cfg.Log.Level = logLevel
	for _, a := range SplitList(udpCL) {
		cfg.Listen.Client = append(cfg.Listen.Client, ListenEntry{Proto: "udp", Addr: a})
	}
	for _, a := range SplitList(tcpCL) {
		cfg.Listen.Client = append(cfg.Listen.Client, ListenEntry{Proto: "tcp", Addr: a})
	}
	for _, a := range SplitList(udpCmdL) {
		cfg.Listen.Commander = append(cfg.Listen.Commander, ListenEntry{Proto: "udp", Addr: a})
	}
	for _, a := range SplitList(tcpCmdL) {
		cfg.Listen.Commander = append(cfg.Listen.Commander, ListenEntry{Proto: "tcp", Addr: a})
	}

	// max-wrong "N,SEC"
	if maxWrongS != "" {
		parts := strings.Split(maxWrongS, ",")
		if len(parts) != 2 {
			return nil, fmt.Errorf("--max-wrong must be exactly \"N,SEC\" (got %q)", maxWrongS)
		}
		n, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		s, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || n <= 0 || s <= 0 {
			return nil, fmt.Errorf("--max-wrong must be \"N,SEC\" with positive ints (got %q)", maxWrongS)
		}
		cfg.Auth.MaxWrongAttempts = n
		cfg.Auth.WrongBanSeconds = s
	}

	// Validation: collect ALL errors.
	var errs []string
	if cfg.TLS.Cert == "" {
		errs = append(errs, "--cert is required")
	}
	if cfg.TLS.Key == "" {
		errs = append(errs, "--key is required")
	}
	if len(cfg.Auth.MagicList) == 0 {
		errs = append(errs, "--magic-list is required")
	}
	if cfg.Auth.CommanderPassword == "" {
		errs = append(errs, "--commander-password is required")
	}
	if len(cfg.Listen.Client) == 0 && len(cfg.Listen.Commander) == 0 {
		errs = append(errs, "at least one listen address is required")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("configuration errors:\n  - %s", strings.Join(errs, "\n  - "))
	}

	cfg.Defaults()
	return &cfg, nil
}
