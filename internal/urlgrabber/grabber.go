// Package urlgrabber fetches URLs pointing to NZBs (or NZB archives),
// decompresses them, and invokes a handler for each found NZB.
package urlgrabber

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/dirscanner"
	"github.com/hobeone/gonzbd/internal/types"
)

const (
	// DefaultTimeout is the default HTTP request timeout.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxBytes is the default maximum response body size.
	DefaultMaxBytes = 100 * 1024 * 1024 // 100 MiB
)

// Resolver defines the DNS resolution interface used by Grabber to prevent SSRF DNS-rebinding attacks.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

type defaultResolver struct {
	*net.Resolver
}

func (r defaultResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return r.Resolver.LookupHost(ctx, host)
}

// Config holds configuration for the Grabber.
type Config struct {
	// Timeout is the HTTP request timeout. Defaults to DefaultTimeout if zero.
	Timeout time.Duration
	// MaxBytes is the maximum size of the HTTP response body. Defaults to DefaultMaxBytes if zero.
	MaxBytes int64
	// HTTPClient is the HTTP client to use. If nil, a new client with Config.Timeout is created.
	HTTPClient *http.Client
	// Username and Password override any credentials embedded in the URL.
	Username string
	Password string
	// Logger is the structured logger. If nil, slog.Default() is used.
	Logger *slog.Logger
	// AllowPrivateIPs disables SSRF private-IP blocking. Only set this
	// in tests; production must always leave it false.
	AllowPrivateIPs bool
	// Resolver is the DNS resolver. If nil, defaultResolver is used.
	Resolver Resolver
}

// Handler defines the interface for consuming NZB payloads fetched by the Grabber.
// HandleNZB returns the created job ID (or empty string if not applicable) and an error.
type Handler interface {
	HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error)
}

// Grabber fetches URLs pointing to NZBs (or NZB archives), decompresses them,
// and invokes a handler for each found NZB.
type Grabber struct {
	cfg             Config
	handler         Handler
	client          *http.Client
	logger          *slog.Logger
	allowPrivateIPs bool
	resolver        Resolver
}

// New creates a new Grabber with the given config and handler.
func New(cfg Config, h Handler) *Grabber {
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	// Leaf packages self-scope only when they are the root of the logger chain;
	// a caller-supplied logger is assumed already scoped.
	if cfg.Logger == nil {
		cfg.Logger = slog.Default().With("component", "urlgrabber")
	}
	log := cfg.Logger

	resolver := cfg.Resolver
	if resolver == nil {
		resolver = defaultResolver{net.DefaultResolver}
	}

	dialer := &net.Dialer{
		Timeout: cfg.Timeout,
	}
	allowPrivate := cfg.AllowPrivateIPs

	transport := &http.Transport{
		// Proxy respects HTTP_PROXY, HTTPS_PROXY, and NO_PROXY environment variables.
		// Note: When HTTP_PROXY is set, the proxy performs target DNS resolution,
		// and DialContext IP validation applies to the proxy host endpoint. Private IP
		// filtering in urlgrabber validates connection endpoints, relying on the proxy's
		// own SSRF policy for target URL resolution.
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := resolver.LookupHost(ctx, host)
			if err != nil {
				return nil, err
			}
			validIP, err := selectAndValidateIP(ips, allowPrivate)
			if err != nil {
				return nil, fmt.Errorf("SSRF: %w for host %s", err, host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(validIP, port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		}
	} else {
		// Copy the client to avoid mutating the caller-supplied pointer.
		c := *client
		c.Transport = transport
		client = &c
	}
	// Install redirect validation to prevent SSRF via redirect chains.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if err := validateURL(req.Context(), req.URL, allowPrivate, resolver); err != nil {
			return fmt.Errorf("redirect target blocked: %w", err)
		}
		return nil
	}

	return &Grabber{
		cfg:             cfg,
		handler:         h,
		client:          client,
		logger:          log,
		allowPrivateIPs: allowPrivate,
		resolver:        resolver,
	}
}

// Fetch downloads the URL, decompresses if needed, and invokes the handler for
// each NZB found. Returns the job IDs of created jobs.
//
// Fetch reuses decompression logic from dirscanner by writing the fetched body
// to a temp file. This keeps the API clean and avoids duplicating archive
// handling code, at a negligible performance cost.
func (g *Grabber) Fetch(ctx context.Context, urlStr string, opts types.FetchOptions) ([]string, error) {
	if urlStr == "" {
		return nil, fmt.Errorf("URL is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Parse URL to extract embedded credentials and set auth header if provided.
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	// Validate the URL to prevent SSRF attacks.
	if err := validateURL(ctx, parsedURL, g.allowPrivateIPs, g.resolver); err != nil {
		return nil, fmt.Errorf("URL rejected: %w", err)
	}

	// Use configured credentials if provided.
	username := g.cfg.Username
	password := g.cfg.Password

	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // cleanup of response body

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				if duration, err := parseRetryAfter(retryAfter); err == nil {
					return nil, &RetryAfterError{
						StatusCode: resp.StatusCode,
						Duration:   duration,
					}
				}
			}
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Reject HTML responses (likely a login page or error page).
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/html") {
		return nil, fmt.Errorf("received HTML content type, not NZB")
	}

	// Extract filename from Content-Disposition header or URL path.
	filename := extractFilename(resp, parsedURL)

	// Read the response body with size limit.
	limitedBody := io.LimitReader(resp.Body, g.cfg.MaxBytes+1)
	data, err := io.ReadAll(limitedBody)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if int64(len(data)) > g.cfg.MaxBytes {
		return nil, fmt.Errorf("response body exceeds maximum size limit (%d > %d)", len(data), g.cfg.MaxBytes)
	}

	// Write to a temp file with a proper extension for archive detection.
	// Use the filename's extension to help dirscanner.DetectType() work correctly.
	tmpDir, err := os.MkdirTemp("", "urlgrabber-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // cleanup of temp directory

	filename = filepath.Base(filename)
	tmpPath := filepath.Join(tmpDir, filename)
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write temp file: %w", err)
	}

	// Extract NZBs. ExtractNZBs internally detects the archive type.
	nzbs, err := dirscanner.ExtractNZBs(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("failed to extract NZBs: %w", err)
	}

	// Invoke handler for each NZB found.
	var ids []string
	for i, nzbData := range nzbs {
		// Derive a per-NZB filename if multiple found (e.g., from a zip).
		nzbFilename := filename
		if len(nzbs) > 1 {
			base := strings.TrimSuffix(filename, ".nzb")
			nzbFilename = fmt.Sprintf("%s_%d.nzb", base, i)
		}

		id, err := g.handler.HandleNZB(ctx, nzbFilename, nzbData, opts)
		if err != nil {
			g.logger.Error("urlgrabber: handler failed for NZB in archive",
				slog.String("filename", nzbFilename),
				slog.Any("err", err),
			)
			continue
		}
		if id != "" {
			ids = append(ids, id)
		}
	}

	return ids, nil
}

// extractFilename extracts the filename from the Content-Disposition header,
// falling back to the URL path's basename.
func extractFilename(resp *http.Response, parsedURL *url.URL) string {
	// Try Content-Disposition header.
	disposition := resp.Header.Get("Content-Disposition")
	if disposition != "" {
		filename := extractFromContentDisposition(disposition)
		if filename != "" {
			return filename
		}
	}

	// Fall back to URL path basename.
	filename := filepath.Base(parsedURL.Path)
	if filename == "" || filename == "." || filename == "/" {
		filename = "download.nzb"
	}

	// Ensure .nzb extension.
	if !strings.HasSuffix(strings.ToLower(filename), ".nzb") &&
		!strings.HasSuffix(strings.ToLower(filename), ".nzb.gz") &&
		!strings.HasSuffix(strings.ToLower(filename), ".nzb.bz2") &&
		!strings.HasSuffix(strings.ToLower(filename), ".zip") {
		filename += ".nzb"
	}

	return filename
}

// extractFromContentDisposition extracts the filename from a Content-Disposition header value.
// Uses mime.ParseMediaType to correctly handle quoted semicolons and RFC 2231 encoding.
func extractFromContentDisposition(disposition string) string {
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// validateURL rejects URLs that could be used for SSRF attacks:
//   - Only http and https schemes are allowed.
//   - Hosts that resolve to loopback, link-local, or RFC1918 private
//     addresses are rejected (unless allowPrivateIPs is true).
//   - The hostname "localhost" is explicitly blocked.
func validateURL(ctx context.Context, u *url.URL, allowPrivateIPs bool, resolver Resolver) error {
	// Scheme check.
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}

	// Hostname check.
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("localhost URLs are not allowed")
	}

	if allowPrivateIPs {
		return nil
	}

	// Resolve and check all IPs.
	ips, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("failed to resolve host %q: %w", host, err)
	}
	_, err = selectAndValidateIP(ips, allowPrivateIPs)
	return err
}

// selectAndValidateIP filters and validates host IPs.
func selectAndValidateIP(ips []string, allowPrivate bool) (string, error) {
	if allowPrivate {
		if len(ips) > 0 {
			return ips[0], nil
		}
		return "", fmt.Errorf("no IPs found")
	}

	_, cgNatNet, _ := net.ParseCIDR("100.64.0.0/10")
	var validIP string
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		isPrivate := ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || isSiteLocal(ip) || cgNatNet.Contains(ip)
		if !isPrivate {
			if v4 := to4Safe(ip); v4 != nil {
				isPrivate = v4.IsPrivate() || v4.IsLoopback() || v4.IsLinkLocalUnicast() || v4.IsUnspecified() || cgNatNet.Contains(v4)
			}
		}
		if isPrivate {
			return "", fmt.Errorf("resolves to private/loopback/unspecified/local: %s", ipStr)
		}
		if validIP == "" {
			validIP = ipStr
		}
	}
	if validIP == "" {
		return "", fmt.Errorf("no valid IP found")
	}
	return validIP, nil
}

// to4Safe converts IPv4-mapped and IPv4-compatible IPv6 addresses to IPv4.
func to4Safe(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	// Handle IPv4-compatible IPv6 addresses (first 12 bytes are 0).
	if len(ip) == 16 &&
		ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] == 0 &&
		ip[4] == 0 && ip[5] == 0 && ip[6] == 0 && ip[7] == 0 &&
		ip[8] == 0 && ip[9] == 0 && ip[10] == 0 && ip[11] == 0 {
		return ip[12:16]
	}
	return nil
}

// isSiteLocal IPv6 checks if the address is within fec0::/10.
func isSiteLocal(ip net.IP) bool {
	return len(ip) == 16 && ip[0] == 0xfe && (ip[1]&0xc0) == 0xc0
}

// RetryAfterError is returned when HTTP 429 or 503 is encountered with a Retry-After header.
type RetryAfterError struct {
	StatusCode int
	Duration   time.Duration
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("HTTP %d: retry after %s", e.StatusCode, e.Duration)
}

func parseRetryAfter(val string) (time.Duration, error) {
	if seconds, err := strconv.Atoi(val); err == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	t, err := http.ParseTime(val)
	if err != nil {
		return 0, err
	}
	duration := max(time.Until(t), 0)
	return duration, nil
}
