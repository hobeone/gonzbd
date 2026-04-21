package api

import (
	"log/slog"
	"net"
	"net/http"
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
	if cfg.LocalhostBypass && isLocalhost(r) {
		return LevelAdmin
	}
	key := apiKeyFromRequest(r)
	if key == "" {
		return 0
	}
	if key == cfg.APIKey {
		return LevelAdmin
	}
	if key == cfg.NZBKey {
		return LevelProtected
	}
	return 0
}

// apiKeyFromRequest extracts the API key from (in priority order):
//  1. ?apikey= query parameter
//  2. POST form field "apikey"
//  3. X-API-Key header
//  4. "sab_apikey" cookie (set by the SPA handler)
func apiKeyFromRequest(r *http.Request) string {
	if k := r.FormValue("apikey"); k != "" {
		return k
	}
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if c, err := r.Cookie("sab_apikey"); err == nil {
		return c.Value
	}
	return ""
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

// maxFormBytes caps the request body for non-upload form parsing to prevent
// memory exhaustion. Multipart file uploads (Content-Type: multipart/form-data)
// are exempted here — the upload handler (modeAddFile) sets its own limit
// via MaxBytesReader before parsing the multipart body.
const maxFormBytes = 2 * 1024 * 1024 // 2 MiB

func isMultipartUpload(r *http.Request) bool {
	return r.Method == http.MethodPost &&
		strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// loggingMiddleware records each request at Info level with method, path,
// status, and duration.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMultipartUpload(r) {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		mode := r.FormValue("mode")   //nolint:gosec // G120: body already limited above
		action := r.FormValue("name") //nolint:gosec // G120: body already limited above

		attrs := []any{
			"component", "api",
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
		slog.Info("api", attrs...)
	})
}

// sanitizeQuery redacts apikey/nzbkey values from the query string so they
// don't leak into logs. Other parameters are preserved for debugging.
func sanitizeQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		if strings.HasPrefix(p, "apikey=") || strings.HasPrefix(p, "nzbkey=") {
			eq := strings.IndexByte(p, '=')
			parts[i] = p[:eq+1] + "***"
		}
	}
	return strings.Join(parts, "&")
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
