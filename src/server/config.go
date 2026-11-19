// Package server implements she11_server: pure relay, registry, auth, audit.
package server

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// ListenEntry is one listen socket ("both" proto is expanded by callers).
type ListenEntry struct {
	Proto string `toml:"proto"` // udp | tcp
	Addr  string `toml:"addr"`
}

// Config mirrors loadable demo/04_she11_server.toml (CLI flags build it too).
type Config struct {
	Identity struct {
		Name       string `toml:"name"`
		PublicAddr string `toml:"public_addr"`
	} `toml:"identity"`
	Listen struct {
		Client    []ListenEntry `toml:"client"`
		Commander []ListenEntry `toml:"commander"`
	} `toml:"listen"`
	TLS struct {
		Cert string `toml:"cert"`
		Key  string `toml:"key"`
	} `toml:"tls"`
	Auth struct {
		MagicList         []string `toml:"magic_list"`
		CommanderPassword string   `toml:"commander_password"`
		MaxWrongAttempts  int      `toml:"max_wrong_attempts"`
		WrongBanSeconds   int      `toml:"wrong_ban_seconds"`
	} `toml:"auth"`
	Limits struct {
		MaxCommander     int `toml:"max_commander"`
		MaxClients       int `toml:"max_clients"`
		MaxSessionsTotal int `toml:"max_sessions_total"`
		OpenShellTimeout int `toml:"open_shell_timeout"`
	} `toml:"limits"`
	Log struct {
		Folder string `toml:"folder"`
		Level  string `toml:"level"`
	} `toml:"log"`
}

// Defaults fills zero values with defaults.
func (c *Config) Defaults() {
	if c.Auth.MaxWrongAttempts == 0 {
		c.Auth.MaxWrongAttempts = 3
	}
	if c.Auth.WrongBanSeconds == 0 {
		c.Auth.WrongBanSeconds = 60
	}
	if c.Limits.MaxClients == 0 {
		c.Limits.MaxClients = 2000
	}
	if c.Limits.OpenShellTimeout == 0 {
		c.Limits.OpenShellTimeout = 5
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Identity.Name == "" {
		if hn, err := os.Hostname(); err == nil {
			c.Identity.Name = hn
		} else {
			c.Identity.Name = "she11-server"
		}
	}
}

// LoadTOML reads the config file (all defaults applied).
func LoadTOML(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Defaults()
	return &c, nil
}

// SplitList parses comma-separated listen/values lists.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
