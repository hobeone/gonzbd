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

	// LocalRanges lists additional client IP ranges (CIDRs like
	// "10.0.0.0/8", or bare IPs) that are trusted to receive and use the
	// web UI's ephemeral session cookie, in ADDITION to loopback (which is
	// always trusted). Loopback-only is the default: a non-loopback client
	// (LAN, Docker bridge, reverse proxy) is NOT handed an admin session
	// cookie unless its range is listed here. Set this to your reverse
	// proxy / Docker network CIDR to allow the UI to work for those clients
	// without entering a key. Does not affect API-key/NZB-key auth, which
	// works from any address.
	LocalRanges []string `yaml:"local_ranges" json:"local_ranges"`
	// VerifyXFFHeader, when true, additionally validates the
	// X-Forwarded-For/Forwarded/X-Real-IP chain: once the direct peer
	// qualifies as trusted (loopback or LocalRanges), every forwarded hop
	// must also be trusted or the request is treated as untrusted. When
	// false (the default) and no forwarding header is present, trust is
	// based on the peer address alone — but if any such header IS present
	// while this is false, the request fails closed rather than trusting
	// the (proxy-controlled) peer address blindly: a same-host reverse
	// proxy makes every request arrive with a loopback remote address
	// regardless of the real client, so an unverified peer proves nothing
	// once a forwarding header shows a proxy is interposed. Set this to
	// true and add the proxy's address to LocalRanges to trust it
	// explicitly. The header is never consulted when the direct peer is
	// untrusted, so it cannot be spoofed by a public client.
	VerifyXFFHeader bool `yaml:"verify_xff_header" json:"verify_xff_header"`
	// TrustedForwardHeader names the single forwarding header your reverse
	// proxy actually manages (sets/overwrites reliably), consulted when
	// VerifyXFFHeader is true: "x-forwarded-for" (default when empty),
	// "forwarded", or "x-real-ip". Only this header is ever validated for
	// hop-trust purposes — the other two are ignored even if present. This
	// matters because a proxy that reliably overwrites one header may pass
	// another through from the client untouched; without an explicit
	// selection, a client could inject a forged value in the unmanaged
	// header and have it override the proxy's real signal. Mirrors nginx's
	// real_ip_header directive, which requires the same explicit choice.
	TrustedForwardHeader TrustedForwardHeader `yaml:"trusted_forward_header" json:"trusted_forward_header"`

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
	// LogDir holds the server's own log files.
	LogDir string `yaml:"log_dir" json:"log_dir"`
	// AdminDir holds queue / state files.
	AdminDir string `yaml:"admin_dir" json:"admin_dir"`
	// LogLevel is the minimum log level: "debug", "info", "warn", or "error".
	// Empty string defaults to "info".
	LogLevel string `yaml:"log_level" json:"log_level"`

	// LogLevels overrides the global LogLevel for specific components.
	// Keys are component names (e.g., "api", "downloader", "nntp").
	// Values are log levels: "debug", "info", "warn", "error", or "off".
	// Components not listed inherit the global LogLevel.
	//
	// Example:
	//   log_levels:
	//     api: warn          # suppress routine API chatter
	//     downloader: debug  # verbose downloader output
	//     nntp: error        # only errors from NNTP connections
	LogLevels map[string]string `yaml:"log_levels" json:"log_levels"`

	// Language is reserved for future UI translation support.
	// Currently unused — the UI is English-only. Default: "en".
	Language string `yaml:"language" json:"language"`
}

// LevelOff is a log level that suppresses all output for a component.
// It is higher than slog.LevelError so no record can pass.
const LevelOff = slog.Level(99)

// ParseLogLevel decodes the LogLevel string to an slog.Level.
// Empty string returns LevelInfo. Accepts case-insensitive "debug",
// "info", "warn", "error", "off". Returns an error for invalid input.
func (g *GeneralConfig) ParseLogLevel() (slog.Level, error) {
	return ParseLevel(g.LogLevel)
}

// ParseLogLevels parses the per-component LogLevels map into a
// map[string]slog.Level suitable for the filter handler.
// Returns an error if any value is not a valid log level.
func (g *GeneralConfig) ParseLogLevels() (map[string]slog.Level, error) {
	if len(g.LogLevels) == 0 {
		return nil, nil
	}
	m := make(map[string]slog.Level, len(g.LogLevels))
	for component, levelStr := range g.LogLevels {
		lvl, err := ParseLevel(levelStr)
		if err != nil {
			return nil, err
		}
		m[component] = lvl
	}
	return m, nil
}

// ParseLevel decodes a level string. Accepts case-insensitive "debug",
// "info", "warn", "error", "off". Empty returns LevelInfo.
// Returns an error for invalid input.
func ParseLevel(s string) (slog.Level, error) {
	if s == "" {
		return slog.LevelInfo, nil
	}
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	case "off":
		return LevelOff, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (must be debug, info, warn, error, or off)", s)
	}
}
