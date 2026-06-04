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
}

// New creates a new Grabber with the given config and handler.
func New(cfg Config, h Handler) *Grabber {
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	log := cfg.Logger.With("component", "urlgrabber")

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	allowPrivate := cfg.AllowPrivateIPs
	// Install redirect validation to prevent SSRF via redirect chains.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if err := validateURL(req.URL, allowPrivate); err != nil {
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
	if err := validateURL(parsedURL, g.allowPrivateIPs); err != nil {
		return nil, fmt.Errorf("URL rejected: %w", err)
	}

	// Use configured credentials if provided, otherwise try URL userinfo.
	username := g.cfg.Username
	password := g.cfg.Password
	if username == "" && password == "" {
		if parsedURL.User != nil {
			username = parsedURL.User.Username()
			password, _ = parsedURL.User.Password()
		}
	}

	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // cleanup of response body

	if resp.StatusCode != http.StatusOK {
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

// isPrivateIP returns true if ip falls within any private, loopback, or
// link-local address range. Uses stdlib methods available since Go 1.17.
func isPrivateIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// validateURL rejects URLs that could be used for SSRF attacks:
//   - Only http and https schemes are allowed.
//   - Hosts that resolve to loopback, link-local, or RFC1918 private
//     addresses are rejected (unless allowPrivateIPs is true).
//   - The hostname "localhost" is explicitly blocked.
func validateURL(u *url.URL, allowPrivateIPs bool) error {
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
	//nolint:gosec // false positive: this function is the SSRF validator checking for private/loopback IPs
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host %q: %w", host, err)
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip != nil && isPrivateIP(ip) {
			return fmt.Errorf("host %q resolves to private/loopback address %s", host, ipStr)
		}
	}

	return nil
}
