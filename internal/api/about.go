package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// modeAbout returns system information for the About dialog: version,
// local/public IPv4, hostname, configured directory paths, and
// resolved binary paths for post-processing tools.
func (s *Server) modeAbout(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname() //nolint:errcheck // display only; a failure shows an empty hostname

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
		snap := s.config.Snapshot()
		gen := &snap.General
		pp := &snap.PostProc
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

	// Look up the public IPv4 and IPv6 concurrently; a cache hit costs no
	// request, and a miss is bounded by publicIP's 3s timeout.
	var publicV4, publicV6 string
	var ipWG sync.WaitGroup
	ipWG.Add(2)
	ctx := r.Context()
	go func() { defer ipWG.Done(); publicV4 = s.publicIPs.v4(ctx) }()
	go func() { defer ipWG.Done(); publicV6 = s.publicIPs.v6(ctx) }()
	ipWG.Wait()

	about := map[string]any{
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
	s.writeBuildFields(about)

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

const (
	publicIPv4URL = "https://api.ipify.org?format=text"
	publicIPv6URL = "https://api6.ipify.org?format=text"

	// publicIPTTL is how long a valid address is reused.
	publicIPTTL = 10 * time.Minute
	// publicIPNegativeTTL is how long a failed or invalid lookup is
	// remembered. It is short on purpose: caching "" for publicIPTTL would
	// hide a transient failure for ten minutes, but caching nothing would
	// make every call against a dead endpoint cost the full 3s timeout. A
	// host with no IPv6 route hits exactly that case on every call.
	publicIPNegativeTTL = 30 * time.Second
)

// publicIPSlot caches one address family's lookup. mu guards val and expires
// only and is never held across the fetch; flight makes concurrent callers
// on an expired slot share one request instead of each making their own.
type publicIPSlot struct {
	mu      sync.Mutex
	val     string
	expires time.Time
	flight  singleflight.Group
}

// fresh returns the cached value and whether it is still within its TTL.
func (sl *publicIPSlot) fresh(now time.Time) (string, bool) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return sl.val, now.Before(sl.expires)
}

// publicIPCache caches the public-address lookups for mode=about. The zero
// value is ready to use: a nil fetch means publicIP and a nil now means
// time.Now. Tests set both so they never touch the network or sleep.
type publicIPCache struct {
	fetch  func(ctx context.Context, urlStr string) string
	now    func() time.Time
	v4Slot publicIPSlot
	v6Slot publicIPSlot
}

func (c *publicIPCache) v4(ctx context.Context) string {
	return c.get(ctx, &c.v4Slot, publicIPv4URL, false)
}

func (c *publicIPCache) v6(ctx context.Context) string {
	return c.get(ctx, &c.v6Slot, publicIPv6URL, true)
}

// inFamily reports whether a belongs to the IPv6 family when wantV6 is set,
// else to IPv4. A 4in6-mapped address is IPv4, so it is never IPv6 here.
func inFamily(a netip.Addr, wantV6 bool) bool {
	if wantV6 {
		return a.Is6() && !a.Is4In6()
	}
	return a.Is4()
}

func (c *publicIPCache) get(ctx context.Context, slot *publicIPSlot, urlStr string, wantV6 bool) string {
	now := c.now
	if now == nil {
		now = time.Now
	}
	fetch := c.fetch
	if fetch == nil {
		fetch = publicIP
	}

	if val, ok := slot.fresh(now()); ok {
		return val
	}

	ch := slot.flight.DoChan("", func() (any, error) {
		// A caller that checked just before the previous flight stored its
		// result lands here as a new flight; do not fetch again.
		if val, ok := slot.fresh(now()); ok {
			return val, nil
		}
		// The result is shared with every caller of this flight, so one
		// caller's cancellation must not turn it into a cached "".
		raw := fetch(context.WithoutCancel(ctx), urlStr)
		val := ""
		if a, err := netip.ParseAddr(raw); err == nil && a.Zone() == "" && inFamily(a, wantV6) {
			val = a.String()
		}
		ttl := publicIPNegativeTTL
		if val != "" {
			ttl = publicIPTTL
		}
		slot.mu.Lock()
		slot.val, slot.expires = val, now().Add(ttl)
		slot.mu.Unlock()
		return val, nil
	})
	select {
	case res := <-ch:
		if val, ok := res.Val.(string); ok {
			return val
		}
		return ""
	case <-ctx.Done():
		return ""
	}
}

// publicIP queries an external service URL to determine a public IP
// address. This is best-effort: on any failure (timeout, network error,
// non-200 response) it returns "". It returns the body unvalidated;
// publicIPCache.get checks it is an address of the right family. Used with
// api.ipify.org for IPv4 and api6.ipify.org for IPv6.
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
