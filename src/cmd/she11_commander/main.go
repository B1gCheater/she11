// Command she11_commander is the operator console.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"she11/src/commander"
	"she11/src/shared/lock"
)

// mutexName is injected at build time by builder.py (--mutex).
var mutexName string

const usage = `she11_commander — operator console: list, probe, interactive shell, MCP mode
                  pure outbound client, never accepts inbound connections

usage:
  she11_commander [-c CONFIG] [options]
  she11_commander [-c CONFIG] <command> [command options]

config:
  -c, --config PATH          (default lookup: ./she11_commander.toml, portable)

global options:
  -j, --json                 machine-readable output
  --timeout SEC              operation timeout (default: 10)
  -h, --help

commands:
  --check-server [PASS:ADDR,...]     probe server health
  --list-client [PASS:ADDR,...]      list clients (merged view)
  --start-shell TARGET               interactive shell
      --shell-type TYPE              pwsh|powershell|cmd|fish|zsh|bash|sh|default
      --server PASS:ADDR,...         servers to use (config fallback)
      --tcp | --udp                  force transport
  --exec TARGET -- CMD               one-shot command
  --mcp {stdio|http}                 MCP server mode (stdio recommended)

examples:
  she11_commander --check-server
  she11_commander --list-client "Str0ng:hk.example.com:2079"
  she11_commander --start-shell k7f2qm
  she11_commander --exec k7f2qm -- "df -h"
  she11_commander --mcp stdio
`

func main() {
	fs := flag.NewFlagSet("she11_commander", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		configPath string
		jsonOut    bool
		timeoutSec int
		help       bool

		checkServer bool
		listClient  bool
		checkInline string
		listInline  string
		startTarget string
		shellType   string
		serverArgs  string
		forceProto  string // "" | "udp" | "tcp"
		execMode    bool
		execTarget  string
		execCmd     string
		mcpMode     string
		mcpListen   string
		mcpToken    string
	)
	fs.StringVar(&configPath, "c", "", "")
	fs.StringVar(&configPath, "config", "", "")
	fs.BoolVar(&jsonOut, "j", false, "")
	fs.BoolVar(&jsonOut, "json", false, "")
	fs.IntVar(&timeoutSec, "timeout", 0, "")
	fs.BoolVar(&help, "h", false, "")
	fs.BoolVar(&help, "help", false, "")

	fs.BoolVar(&checkServer, "check-server", false, "")
	fs.BoolVar(&listClient, "list-client", false, "")
	fs.StringVar(&startTarget, "start-shell", "", "")
	fs.StringVar(&shellType, "shell-type", "", "")
	fs.StringVar(&serverArgs, "server", "", "")
	fs.BoolVar(&execMode, "exec", false, "exec one-shot command")
	fs.StringVar(&mcpMode, "mcp", "", "")
	fs.StringVar(&mcpListen, "mcp-listen", "", "")
	fs.StringVar(&mcpToken, "mcp-token", "", "")

	// --tcp/--udp handled via fs.Visit
	var forceTCP, forceUDP bool
	fs.BoolVar(&forceTCP, "tcp", false, "")
	fs.BoolVar(&forceUDP, "udp", false, "")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if help {
		fmt.Print(usage)
		os.Exit(0)
	}
	if forceTCP && forceUDP {
		fmt.Fprintln(os.Stderr, "--tcp and --udp are mutually exclusive")
		os.Exit(2)
	}
	if forceTCP {
		forceProto = "tcp"
	}
	if forceUDP {
		forceProto = "udp"
	}
	// positional leftovers: first token after --check-server/--list-client is
	// the inline PASS:ADDR list (optional)
	rest := fs.Args()
	if checkServer && len(rest) > 0 && rest[0] != "" && !strings.HasPrefix(rest[0], "-") {
		checkInline = rest[0]
		rest = rest[1:]
	}
	if listClient && len(rest) > 0 && rest[0] != "" && !strings.HasPrefix(rest[0], "-") {
		listInline = rest[0]
		rest = rest[1:]
	}
	if execMode {
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "--exec requires TARGET -- CMD")
			os.Exit(2)
		}
		execTarget = rest[0]
		if len(rest) >= 3 && rest[1] == "--" {
			execCmd = strings.Join(rest[2:], " ")
		} else if len(rest) >= 2 && strings.HasPrefix(rest[1], "--") {
			execCmd = strings.Join(rest[2:], " ")
		} else if len(rest) >= 2 {
			execCmd = strings.Join(rest[1:], " ")
		}
		if execCmd == "" {
			fmt.Fprintln(os.Stderr, "--exec requires a command after TARGET")
			os.Exit(2)
		}
	}

	// config resolution
	var cfg commander.Config
	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = commander.DefaultConfigPath()
	}
	if cfgPath != "" {
		c, err := commander.LoadTOML(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		cfg = *c
	}
	if timeoutSec > 0 {
		cfg.General.Timeout = timeoutSec
	}
	cfg.Defaults()

	c, err := commander.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	inline := func(s string) []string {
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		var out []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	// mutex (default off, compile-time injected)
	release, err := lock.Acquire(mutexName)
	if err == lock.ErrHeld {
		fmt.Fprintln(os.Stderr, "she11_commander: another instance is already running (mutex)")
		os.Exit(9)
	}
	defer release()

	// detect which flags were actually present (value-less flags too)
	present := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { present[f.Name] = true })

	if present["mcp"] && mcpMode == "" {
		mcpMode = "stdio"
	}
	if present["start-shell"] && startTarget == "" {
		fmt.Fprintln(os.Stderr, "--start-shell requires a TARGET (client id or ip:port)")
		os.Exit(2)
	}

	inlineOf := firstNonEmpty(checkInline, listInline, serverArgs)
	var code int
	switch {
	case checkServer:
		code = c.CheckServer(c.ResolveServers(inline(inlineOf)), jsonOut)
	case listClient:
		code = c.ListClients(c.ResolveServers(inline(inlineOf)), jsonOut)
	case startTarget != "":
		code = c.StartShell(c.ResolveServers(inline(serverArgs)), startTarget, shellType, forceProto)
	case execMode:
		code = c.Exec(c.ResolveServers(inline(serverArgs)), execTarget, shellType, execCmd, forceProto)
	case present["mcp"]:
		if mcpListen == "" {
			mcpListen = cfg.MCP.Listen
		}
		if mcpToken == "" {
			mcpToken = cfg.MCP.Token
		}
		code = c.MCP(mcpMode, mcpListen, mcpToken)
	default:
		fmt.Print(usage)
		code = 0
	}
	os.Exit(code)
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}
