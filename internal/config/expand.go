package config

import (
	"os"
	"path/filepath"
)

// ExpandPaths traverses the configuration, expands environment variables
// ($VAR, ${VAR}), and replaces leading '~' in path fields with the current
// user's home directory.
func (c *Config) ExpandPaths() {
	c.General.expandPaths()
	c.PostProc.expandPaths()
	c.Notifications.expandPaths()
}

func (g *GeneralConfig) expandPaths() {
	g.HTTPSCert = expandPath(g.HTTPSCert)
	g.HTTPSKey = expandPath(g.HTTPSKey)
	g.DownloadDir = expandPath(g.DownloadDir)
	g.CompleteDir = expandPath(g.CompleteDir)
	g.DirscanDir = expandPath(g.DirscanDir)
	g.ScriptDir = expandPath(g.ScriptDir)
	g.LogDir = expandPath(g.LogDir)
	g.AdminDir = expandPath(g.AdminDir)
}

func (p *PostProcConfig) expandPaths() {
	p.Par2Command = expandPath(p.Par2Command)
	p.UnrarCommand = expandPath(p.UnrarCommand)
	p.SevenzCommand = expandPath(p.SevenzCommand)
}

func (n *NotificationConfig) expandPaths() {
	n.Script.Path = expandPath(n.Script.Path)
}

// expandPath expands environment variables ($VAR, ${VAR}) and replaces
// a leading "~" or "~/" with the user's home directory.
// If home expansion fails (e.g. HOME not set), the path is returned
// with only env-var expansion applied.
func expandPath(path string) string {
	if path == "" {
		return path
	}

	// First, expand environment variables in path fields only.
	// This is intentionally NOT done globally on the raw YAML to
	// avoid corrupting non-path values containing '$' (passwords,
	// API keys, regex patterns).
	path = os.ExpandEnv(path)

	if path == "" || path[0] != '~' {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	if len(path) == 1 {
		return home
	}

	if path[1] == '/' || path[1] == filepath.Separator {
		return filepath.Join(home, path[1:])
	}

	// Paths like ~user are not supported; return unchanged.
	return path
}
