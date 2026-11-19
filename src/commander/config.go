// Package commander implements the operator console.
package commander

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// ServerConf is one server entry in she11_commander.toml.
type ServerConf struct {
	Name     string `toml:"name"`
	Addr     string `toml:"addr"`
	Password string `toml:"password"`
}

// Config mirrors demo/05_she11_commander.toml.
type Config struct {
	General struct {
		ShellType string `toml:"shell_type"`
		Transport string `toml:"transport"`
		Timeout   int    `toml:"timeout"`
		JSON      bool   `toml:"json"`
	} `toml:"general"`
	Servers []ServerConf `toml:"server"`
	MCP     struct {
		Mode   string `toml:"mode"` // off | stdio | http
		Listen string `toml:"listen"`
		Token  string `toml:"token"`
	} `toml:"mcp"`
}

// Defaults fills zero values.
func (c *Config) Defaults() {
	if c.General.ShellType == "" {
		c.General.ShellType = "default"
	}
	if c.General.Transport == "" {
		c.General.Transport = "auto"
	}
	if c.General.Timeout == 0 {
		c.General.Timeout = 10
	}
	if c.MCP.Listen == "" {
		c.MCP.Listen = "127.0.0.1:8899"
	}
}

// LoadTOML reads the commander config.
func LoadTOML(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Defaults()
	return &c, nil
}

// DefaultConfigPath returns ./she11_commander.toml if it exists.
func DefaultConfigPath() string {
	if _, err := os.Stat("she11_commander.toml"); err == nil {
		return "she11_commander.toml"
	}
	return ""
}
