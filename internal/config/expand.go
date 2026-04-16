package config

import (
	"os"
	"path/filepath"
)

// ExpandPaths traverses the configuration and replaces leading '~' in path
// fields with the current user's home directory.
func (c *Config) ExpandPaths() {
	c.General.expandPaths()
	c.PostProc.expandPaths()
}

func (g *GeneralConfig) expandPaths() {
	g.HTTPSCert = expandHome(g.HTTPSCert)
	g.HTTPSKey = expandHome(g.HTTPSKey)
	g.DownloadDir = expandHome(g.DownloadDir)
	g.CompleteDir = expandHome(g.CompleteDir)
	g.DirscanDir = expandHome(g.DirscanDir)
	g.ScriptDir = expandHome(g.ScriptDir)
	g.EmailDir = expandHome(g.EmailDir)
	g.LogDir = expandHome(g.LogDir)
	g.AdminDir = expandHome(g.AdminDir)
}

func (p *PostProcConfig) expandPaths() {
	p.Par2Command = expandHome(p.Par2Command)
	p.UnrarCommand = expandHome(p.UnrarCommand)
	p.SevenzCommand = expandHome(p.SevenzCommand)
}

// expandHome replaces a leading "~" or "~/" with the user's home directory.
// If expansion fails (e.g. HOME not set), the path is returned unchanged.
func expandHome(path string) string {
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
