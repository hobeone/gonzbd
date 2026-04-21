package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// GeneralConfig holds top-level daemon settings: HTTP listen, credentials,
// directory layout, and language. See spec §9.2.
type GeneralConfig struct {
	// Host is the HTTP bind address.
	Host string `yaml:"host" json:"host"`
	// Port is the HTTP port. 1-65535.
	Port int `yaml:"port" json:"port"`
	// HTTPSPort is the HTTPS port. 0 disables HTTPS.
	HTTPSPort int `yaml:"https_port" json:"https_port"`
	// HTTPSCert is the TLS certificate file. Required when HTTPSPort > 0.
	HTTPSCert string `yaml:"https_cert" json:"https_cert"`
	// HTTPSKey is the TLS private key file. Required when HTTPSPort > 0.
	HTTPSKey string `yaml:"https_key" json:"https_key"`

	// APIKey authenticates API requests. 16-character lowercase hex.
	APIKey string `yaml:"api_key" json:"api_key"`
	// NZBKey authenticates NZB-upload API requests. 16-character lowercase hex.
	NZBKey string `yaml:"nzb_key" json:"nzb_key"`
	// Username for the web UI. Empty disables basic auth.
	Username string `yaml:"username" json:"username"`
	// Password for the web UI. Stored as a hashed value (format set by
	// the auth subsystem); plaintext is rejected at validation time
	// when the auth subsystem is wired up.
	Password string `yaml:"password" json:"password"`

	// DownloadDir is the work-in-progress directory for incomplete
	// downloads. Created on startup if it does not exist.
	DownloadDir string `yaml:"download_dir" json:"download_dir"`
	// CompleteDir is the destination for completed jobs.
	CompleteDir string `yaml:"complete_dir" json:"complete_dir"`
	// DirscanDir is the watched folder for drop-in NZB files. Empty
	// disables the scanner.
	DirscanDir string `yaml:"dirscan_dir" json:"dirscan_dir"`
	// DirscanSpeed is the scan interval (seconds, > 0).
	DirscanSpeed int `yaml:"dirscan_speed" json:"dirscan_speed"`
	// ScriptDir holds user post-processing scripts.
	ScriptDir string `yaml:"script_dir" json:"script_dir"`
	// EmailDir holds notification email templates.
	EmailDir string `yaml:"email_dir" json:"email_dir"`
	// LogDir holds the server's own log files.
	LogDir string `yaml:"log_dir" json:"log_dir"`
	// AdminDir holds queue / state files.
	AdminDir string `yaml:"admin_dir" json:"admin_dir"`
	// LogLevel is the minimum log level: "debug", "info", "warn", or "error".
	// Empty string defaults to "info".
	LogLevel string `yaml:"log_level" json:"log_level"`

	// LogAllow restricts logging to only these components (e.g., "downloader").
	// Empty means all components are allowed.
	LogAllow []string `yaml:"log_allow" json:"log_allow"`
	// LogDeny suppresses logging from these components.
	LogDeny []string `yaml:"log_deny" json:"log_deny"`

	// Language is the BCP-47 (or shorter) UI language code.
	Language string `yaml:"language" json:"language"`
}

// ParseLogLevel decodes the LogLevel string to an slog.Level.
// Empty string returns LevelInfo. Accepts case-insensitive "debug",
// "info", "warn", "error". Returns an error for invalid input.
func (g *GeneralConfig) ParseLogLevel() (slog.Level, error) {
	if g.LogLevel == "" {
		return slog.LevelInfo, nil
	}
	switch strings.ToLower(g.LogLevel) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (must be debug, info, warn, or error)", g.LogLevel)
	}
}
