package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// modeAbout returns system information for the About dialog: version,
// local/public IPv4, hostname, configured directory paths, and
// resolved binary paths for post-processing tools.
func (s *Server) modeAbout(w http.ResponseWriter, _ *http.Request) {
	hostname, _ := os.Hostname()

	var (
		downloadDir string
		completeDir string
		adminDir    string
		logDir      string
		dirscanDir  string
		scriptDir   string
		par2Cmd     string
		unrarCmd    string
		sevenzCmd   string
	)
	if s.config != nil {
		s.config.WithRead(func(cfg *config.Config) {
			downloadDir = cfg.General.DownloadDir
			completeDir = cfg.General.CompleteDir
			adminDir = cfg.General.AdminDir
			logDir = cfg.General.LogDir
			dirscanDir = cfg.General.DirscanDir
			scriptDir = cfg.General.ScriptDir
			par2Cmd = cfg.PostProc.Par2Command
			unrarCmd = cfg.PostProc.UnrarCommand
			sevenzCmd = cfg.PostProc.SevenzCommand
		})
	}

	about := map[string]string{
		"version":      s.version,
		"commit":       s.commit,
		"build_date":   s.date,
		"local_ipv4":   localIPv4(),
		"public_ipv4":  publicIP("https://api.ipify.org?format=text"),
		"public_ipv6":  publicIP("https://api6.ipify.org?format=text"),
		"hostname":     hostname,
		"config_path":  s.configPath,
		"download_dir": downloadDir,
		"complete_dir": completeDir,
		"admin_dir":    adminDir,
		"log_dir":      logDir,
		"dirscan_dir":  dirscanDir,
		"script_dir":   scriptDir,
		"par2_path":    resolveBinary(par2Cmd, "par2"),
		"unrar_path":   resolveBinary(unrarCmd, "unrar"),
		"sevenz_path":  resolve7z(sevenzCmd),
	}

	respondOK(w, "about", about)
}

// localIPv4 returns the first non-loopback IPv4 address found on the
// host's network interfaces. Returns "" if none is found.
func localIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		return ip.String()
	}
	return ""
}

// publicIP queries an external service URL to determine a public IP
// address. This is best-effort: on any failure (timeout, network error,
// non-200 response) it returns "". Used with api.ipify.org for IPv4 and
// api6.ipify.org for IPv6.
func publicIP(url string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	// IP addresses are at most 45 bytes (full IPv6 with zones).
	// Read a small bounded amount to avoid unbounded memory usage.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// resolveBinary resolves a binary path. If cfgPath is non-empty it is
// returned directly (user-configured override). Otherwise the fallback
// name is looked up on PATH via exec.LookPath. Returns "" if not found.
func resolveBinary(cfgPath, fallback string) string {
	if cfgPath != "" {
		return cfgPath
	}
	p, err := exec.LookPath(fallback)
	if err != nil {
		return ""
	}
	return p
}

// resolve7z resolves the 7z binary. It first checks the config override,
// then tries "7zz" (modern standalone), then "7z" (classic) via PATH.
func resolve7z(cfgPath string) string {
	if cfgPath != "" {
		return cfgPath
	}
	if p, err := exec.LookPath("7zz"); err == nil {
		return p
	}
	if p, err := exec.LookPath("7z"); err == nil {
		return p
	}
	return ""
}
