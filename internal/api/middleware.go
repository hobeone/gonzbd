package api

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AccessLevel defines the privilege tier required for an API call. Higher
// values demand stronger credentials. Matches the integer semantics from
// Python's _api_table second-element (1, 2, 3).
type AccessLevel int

const (
	// LevelOpen requires no authentication. callerLevel returns 0 when no
	// valid key is supplied, so modes at LevelOpen pass with 0 >= 0.
	LevelOpen AccessLevel = 0
	// LevelProtected requires the full API key OR the NZB key.
	LevelProtected AccessLevel = 2
	// LevelAdmin requires the full API key (shutdown, config, etc.).
	LevelAdmin AccessLevel = 3
)

// AuthConfig supplies the keys and localhost-bypass settings to the auth
// middleware.
type AuthConfig struct {
	// APIKey is the full API key (16-char hex). Required for LevelAdmin
	// and sufficient for all levels.
	APIKey string

	// NZBKey is the upload-only key. Sufficient for LevelOpen and
	// LevelProtected, but not LevelAdmin.
	NZBKey string

	// LocalhostBypass, when true, grants LevelAdmin to any request from
	// 127.0.0.0/8 or ::1. Mirrors Python's local_ranges behavior.
	LocalhostBypass bool
}

// callerLevel determines the highest access level the caller can reach
// based on the supplied credentials and source address.
func callerLevel(r *http.Request, cfg AuthConfig) AccessLevel {
	if cfg.LocalhostBypass && isLocalhost(r) && !isCrossOrigin(r) {
		return LevelAdmin
	}
	key, fromCookie := apiKeyFromRequest(r)
	if key == "" {
		return 0
	}
	// If the key came from a cookie, enforce cross-origin check for
	// defense-in-depth (cookie SameSite may be insufficient on older browsers).
	if fromCookie && isCrossOrigin(r) {
		return 0
	}
	// Use constant-time comparison to prevent timing attacks.
	if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.APIKey)) == 1 {
		return LevelAdmin
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.NZBKey)) == 1 {
		return LevelProtected
	}
	return 0
}

// apiKeyFromRequest extracts the API key from (in priority order):
//  1. ?apikey= URL query parameter
//  2. ?nzbkey= URL query parameter
//  3. X-API-Key header
//  4. "sab_apikey" cookie (set by the SPA handler)
//
// Intentionally uses r.URL.Query() instead of r.FormValue() to avoid
// triggering implicit multipart body parsing (which would use the 32MiB
// default memory limit, defeating our 10MiB cap in modeAddFile).
func apiKeyFromRequest(r *http.Request) (string, bool) {
	q := r.URL.Query()
	if k := q.Get("apikey"); k != "" {
		return k, false
	}
	if k := q.Get("nzbkey"); k != "" {
		return k, false
	}
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k, false
	}
	if c, err := r.Cookie("sab_apikey"); err == nil {
		return c.Value, true
	}
	return "", false
}

// isLocalhost returns true if the request originates from a loopback
// address (IPv4 127.0.0.0/8 or IPv6 ::1).
func isLocalhost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// isCrossOrigin returns true if the request carries an Origin header
// from a non-local origin. Browsers send Origin on cross-origin requests
// (POST, PUT, DELETE, and fetch/XHR GET). We use this to reject CSRF
// attempts that abuse the LocalhostBypass feature — a malicious website
// cannot forge the Origin header.
func isCrossOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin header — fall back to Referer and Sec-Fetch-Site.
		// Cross-origin GET requests (via <img>, <form method=GET>) don't
		// send Origin, but modern browsers do send Sec-Fetch-Site.

		// Check Referer first: if it's present and from a non-local host,
		// treat as cross-origin to block CSRF via embedded resources.
		if ref := r.Header.Get("Referer"); ref != "" {
			if u, err := url.Parse(ref); err == nil && u.Host != "" {
				if !strings.EqualFold(u.Host, r.Host) {
					host := u.Hostname()
					ip := net.ParseIP(host)
					if ip == nil || !ip.IsLoopback() {
						if !strings.EqualFold(host, "localhost") {
							return true
						}
					}
				}
			}
		}

		// Block if explicitly cross-site/cross-origin.
		sfs := r.Header.Get("Sec-Fetch-Site")
		if sfs == "cross-site" || sfs == "cross-origin" {
			return true
		}
		// No Sec-Fetch-Site or same-origin/same-site/none → allow.
		return false
	}
	// Parse the origin and check if it matches the request's Host.
	u, err := url.Parse(origin)
	if err != nil {
		return true // malformed → treat as cross-origin
	}

	// Same-host check: if Origin matches the request's Host header,
	// this is a same-origin request regardless of IP (covers LAN access).
	if strings.EqualFold(u.Host, r.Host) {
		return false
	}

	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return false // localhost origin
	}
	// Also allow "localhost" by name (not just IP).
	if strings.EqualFold(host, "localhost") {
		return false
	}
	return true
}

// maxFormBytes caps the request body for non-upload form parsing to prevent
// memory exhaustion. Multipart file uploads use maxUploadBytes (defined in
// queue.go) instead.
const maxFormBytes = 2 * 1024 * 1024 // 2 MiB

// isMultipartUpload returns true for any multipart/form-data request.
// These are given a higher body-size limit (maxUploadBytes) than regular
// form requests (maxFormBytes) since they may carry NZB file payloads.
func isMultipartUpload(r *http.Request) bool {
	return r.Method == http.MethodPost &&
		strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// loggingMiddleware records each request at Info level with method, path,
// status, and duration.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		// Apply body-size limits before any form parsing. Multipart
		// uploads (NZB files) get a higher limit; all other requests
		// are capped to a small form size to prevent DoS.
		// Pass sw (not w) so MaxBytesReader's 413 response is captured
		// by the status logger.
		if isMultipartUpload(r) {
			r.Body = http.MaxBytesReader(sw, r.Body, maxUploadBytes)
		} else {
			r.Body = http.MaxBytesReader(sw, r.Body, maxFormBytes)
		}
		start := time.Now()
		next.ServeHTTP(sw, r)

		mode := r.URL.Query().Get("mode")
		action := r.URL.Query().Get("name")

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start),
		}
		if mode != "" {
			attrs = append(attrs, "mode", mode)
		}
		if action != "" {
			attrs = append(attrs, "action", action)
		}
		if r.URL.RawQuery != "" {
			attrs = append(attrs, "query", sanitizeQuery(r.URL.RawQuery))
		}
		//nolint:gosec // G706: slog structured fields are not vulnerable to log injection
		s.log.Info("api", attrs...)
	})
}

// sanitizeQuery redacts apikey/nzbkey values from the query string so they
// don't leak into logs. Uses url.ParseQuery to handle URL-encoded parameter
// names (e.g. %61pikey → apikey) that would bypass a raw string prefix check.
func sanitizeQuery(raw string) string {
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		// Unparseable query — redact entirely to be safe.
		return "***"
	}
	for key := range parsed {
		lower := strings.ToLower(key)
		if lower == "apikey" || lower == "nzbkey" {
			parsed.Set(key, "***")
		}
	}
	return parsed.Encode()
}

// statusWriter wraps ResponseWriter to capture the status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Hijack implements the http.Hijacker interface.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}
