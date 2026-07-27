package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// modeAbout returns system information for the About dialog: version,
// local/public IPv4, hostname, configured directory paths, and
// resolved binary paths for post-processing tools.
func (s *Server) modeAbout(w http.ResponseWriter, r *http.Request) {
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
		gen := s.config.GetGeneral()
		pp := s.config.GetPostProc()
		downloadDir = gen.DownloadDir
		completeDir = gen.CompleteDir
		adminDir = gen.AdminDir
		logDir = gen.LogDir
		dirscanDir = gen.DirscanDir
		scriptDir = gen.ScriptDir
		par2Cmd = pp.Par2Command
		unrarCmd = pp.UnrarCommand
		sevenzCmd = pp.SevenzCommand
	}

	// Fetch public IPv4 and IPv6 concurrently to halve the worst-case
	// latency from ~6s (two sequential 3s-timeout calls) to ~3s.
	var publicV4, publicV6 string
	var ipWG sync.WaitGroup
	ipWG.Add(2)
	ctx := r.Context()
	go func() { defer ipWG.Done(); publicV4 = publicIP(ctx, "https://api.ipify.org?format=text") }()
	go func() { defer ipWG.Done(); publicV6 = publicIP(ctx, "https://api6.ipify.org?format=text") }()
	ipWG.Wait()

	about := map[string]string{
		"version":      s.version,
		"commit":       s.commit,
		"build_date":   s.date,
		"go_version":   runtime.Version(),
		"local_ipv4":   localIPv4(),
		"public_ipv4":  publicV4,
		"public_ipv6":  publicV6,
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
		"sevenz_path":  resolveBinary(sevenzCmd, unpack.SevenZipBinaries...),
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
func publicIP(ctx context.Context, urlStr string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, http.NoBody)
	if err != nil {
		return ""
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }() // cleanup error in defer

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

// resolveBinary resolves a binary path for display. When cfgPath is set,
// it is resolved via exec.LookPath so that the path is validated.
// When cfgPath is empty, it tries the provided fallbacks in order.
// Returns "" if no binary can be found.
func resolveBinary(cfgPath string, fallbacks ...string) string {
	if cfgPath != "" {
		p, err := exec.LookPath(cfgPath)
		if err != nil {
			return ""
		}
		return p
	}
	for _, name := range fallbacks {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}
