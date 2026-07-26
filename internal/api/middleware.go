package api

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
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

// AuthConfig supplies the keys to the auth middleware.
type AuthConfig struct {
	// APIKey is the full API key (16-char hex). Required for LevelAdmin
	// and sufficient for all levels. Presented via query param, header,
	// or POST form body (third-party clients) — never via cookie.
	APIKey string

	// NZBKey is the upload-only key. Sufficient for LevelOpen and
	// LevelProtected, but not LevelAdmin.
	NZBKey string

	// SessionKey is the ephemeral, in-memory credential issued to the
	// embedded web SPA via the gonzbd_apikey cookie (see Server.sessionKey).
	// It is generated fresh on every process start and is intentionally
	// kept separate from APIKey: a leaked browser cookie cannot be
	// replayed as the permanent key used by third-party integrations
	// (Sonarr, Radarr, etc.), and it stops working on the next restart.
	SessionKey string

	// TrustedRanges are the client IP prefixes (in addition to loopback,
	// which is always trusted) from which the session cookie is accepted as
	// admin. Sourced from General.LocalRanges. Empty means loopback-only.
	// The cookie path is refused for any request whose source is not
	// trusted, so an unauthenticated non-loopback caller cannot obtain
	// admin by replaying a leaked or forged session cookie.
	TrustedRanges []netip.Prefix
	// VerifyXFF mirrors General.VerifyXFFHeader: when true, every
	// X-Forwarded-For hop must also be trusted once the peer qualifies.
	VerifyXFF bool
	// ForwardHeader mirrors General.TrustedForwardHeader: which single
	// forwarding header is consulted when VerifyXFF is true.
	ForwardHeader config.TrustedForwardHeader

	// Logger receives an actionable line whenever the cookie path is
	// refused because config.IsTrustedRemote rejected the source. Falls
	// back to slog.Default() when nil, matching api.New's convention.
	Logger *slog.Logger
}

// callerLevel determines the highest access level the caller can reach
// based on the supplied credentials.
func callerLevel(r *http.Request, cfg AuthConfig) AccessLevel {
	key, fromCookie := apiKeyFromRequest(r)
	if key == "" {
		return 0
	}
	if fromCookie {
		// Enforce cross-origin check for defense-in-depth (cookie
		// SameSite may be insufficient on older browsers).
		if isCrossOrigin(r, cfg) {
			return 0
		}
		// Refuse the cookie path for untrusted sources. The SPA only
		// hands the session cookie to trusted clients (loopback or a
		// configured local_range); enforcing the same gate here means a
		// leaked or forged cookie replayed from a non-loopback address
		// (the SEC-1 attack: GET / to grab the cookie, then replay it)
		// is rejected. Without this, any caller that can reach the port
		// obtained zero-credential admin.
		fh := config.ForwardedHeaders{
			XForwardedFor: r.Header.Get("X-Forwarded-For"),
			Forwarded:     r.Header.Get("Forwarded"),
			XRealIP:       r.Header.Get("X-Real-IP"),
		}
		if trusted, reason := config.IsTrustedRemote(r.RemoteAddr, fh, cfg.TrustedRanges, cfg.VerifyXFF, cfg.ForwardHeader); !trusted {
			log := cfg.Logger
			if log == nil {
				log = slog.Default()
			}
			log.Warn("refused session cookie: source not trusted", "remote_addr", r.RemoteAddr, "reason", reason)
			return 0
		}
		// Cookie-sourced keys are validated against the ephemeral
		// session key only — never against APIKey/NZBKey. See
		// AuthConfig.SessionKey.
		if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.SessionKey)) == 1 {
			return LevelAdmin
		}
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
//  4. "gonzbd_apikey" cookie (set by the SPA handler)
//  5. POST form body: apikey or nzbkey field (third-party app compat)
//
// Steps 1-4 intentionally use r.URL.Query() and headers instead of
// r.FormValue() to avoid triggering implicit multipart body parsing
// before authentication is checked. Step 5 only fires for POST
// requests after the body-size limit is already enforced by
// the middleware's MaxBytesReader.
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
	if c, err := r.Cookie("gonzbd_apikey"); err == nil {
		return c.Value, true
	}
	// Fall back to POST form body for third-party app compatibility.
	// Body-size limits are already enforced by the middleware.
	if r.Method == http.MethodPost {
		if k := formValue(r, "apikey"); k != "" {
			return k, false
		}
		if k := formValue(r, "nzbkey"); k != "" {
			return k, false
		}
	}
	return "", false
}

// splitHostPort splits a "host:port" string into its parts.
// Delegates to config.SplitHostPortTolerant.
func splitHostPort(hostport string) (host, port string) {
	return config.SplitHostPortTolerant(hostport)
}

// isLoopbackHostname reports whether host is a loopback address (127.0.0.0/8,
// ::1) or the literal name "localhost".
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackHostsEquivalent reports whether a and b name the same loopback
// interface (e.g. "localhost" and "127.0.0.1"), independent of case. Two
// hosts that merely both happen to be loopback but aren't the same literal
// host are still treated as equivalent here — port equality (checked by the
// caller) is what distinguishes a same-instance request from a different
// server also listening on loopback.
func loopbackHostsEquivalent(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	return isLoopbackHostname(a) && isLoopbackHostname(b)
}

// defaultPortForScheme returns the standard HTTP/HTTPS default port ("80" or "443")
// for a given scheme name, or empty string for unknown/empty schemes.
func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// splitHostPortNormalized splits hostport and fills an elided port with defaultScheme's
// standard port ("80" for "http", "443" for "https") if no explicit port is present.
func splitHostPortNormalized(hostport, defaultScheme string) (host, port string) {
	h, p := splitHostPort(hostport)
	if p == "" && defaultScheme != "" {
		p = defaultPortForScheme(defaultScheme)
	}
	return h, p
}

// requestScheme returns "https" if r uses TLS directly. If r.TLS is nil and the
// request originates from a trusted source (per config.IsTrustedRemote), it
// also inspects forwarding headers (X-Forwarded-Proto, X-Forwarded-Scheme,
// Forwarded) for an "https" scheme; otherwise returns "http".
func requestScheme(r *http.Request, cfg AuthConfig) string {
	if r.TLS != nil {
		return "https"
	}
	fh := config.ForwardedHeaders{
		XForwardedFor: r.Header.Get("X-Forwarded-For"),
		Forwarded:     r.Header.Get("Forwarded"),
		XRealIP:       r.Header.Get("X-Real-IP"),
	}
	if trusted, _ := config.IsTrustedRemote(r.RemoteAddr, fh, cfg.TrustedRanges, cfg.VerifyXFF, cfg.ForwardHeader); trusted {
		if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") ||
			strings.EqualFold(r.Header.Get("X-Forwarded-Scheme"), "https") {
			return "https"
		}
		if fwd := r.Header.Get("Forwarded"); fwd != "" {
			for part := range strings.SplitSeq(fwd, ";") {
				part = strings.TrimSpace(part)
				if strings.HasPrefix(strings.ToLower(part), "proto=") {
					val := strings.TrimPrefix(part, "proto=")
					val = strings.Trim(val, `"`)
					if strings.EqualFold(val, "https") {
						return "https"
					}
				}
			}
		}
	}
	return "http"
}

// sameOrigin reports whether hostportA (with schemeA) and hostportB (with schemeB)
// should be treated as the same origin: either an exact case-insensitive match,
// or a matching loopback/"localhost" alias on the *same port* — a different port
// always means a different server, even on loopback. Implicit default ports (80/443)
// are normalized based on scheme. See https://github.com/hobeone/gonzbd/issues/103
// and https://github.com/hobeone/gonzbd/issues/134.
func sameOrigin(hostportA, schemeA, hostportB, schemeB string) bool {
	if schemeA != "" && schemeB != "" && !strings.EqualFold(schemeA, schemeB) {
		return false
	}
	aHost, aPort := splitHostPortNormalized(hostportA, schemeA)
	bHost, bPort := splitHostPortNormalized(hostportB, schemeB)
	return aPort == bPort && loopbackHostsEquivalent(aHost, bHost)
}

// isRefererCrossOrigin parses referer and returns true if it originates from
// a host:port different from r.Host (scheme-aware).
func isRefererCrossOrigin(referer string, r *http.Request, cfg AuthConfig) bool {
	if referer == "" {
		return false
	}
	u, err := url.Parse(referer)
	if err != nil || u.Host == "" {
		return false
	}
	return !sameOrigin(u.Host, u.Scheme, r.Host, requestScheme(r, cfg))
}

// isSecFetchSiteCrossOrigin returns true if sfs indicates a cross-site or
// cross-origin request.
func isSecFetchSiteCrossOrigin(sfs string) bool {
	return sfs == "cross-site" || sfs == "cross-origin"
}

// isCrossOrigin returns true if the request carries an Origin header
// from a non-local origin. Browsers send Origin on cross-origin requests
// (POST, PUT, DELETE, and fetch/XHR GET). We use this to reject CSRF
// attempts — a malicious website cannot forge the Origin header.
func isCrossOrigin(r *http.Request, cfg AuthConfig) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin header — fall back to Referer and Sec-Fetch-Site.
		referer := r.Header.Get("Referer")
		if referer == "" {
			_, fromCookie := apiKeyFromRequest(r)
			if fromCookie && isStateChangingRequest(r) {
				return true
			}
		}
		if isRefererCrossOrigin(referer, r, cfg) {
			return true
		}
		return isSecFetchSiteCrossOrigin(r.Header.Get("Sec-Fetch-Site"))
	}

	// Parse the origin and check if it matches the request's Host.
	u, err := url.Parse(origin)
	if err != nil {
		return true // malformed → treat as cross-origin
	}

	// Both Origin and Sec-Fetch-Site must agree the request is same-origin;
	// checking only one leaves the other spoofable/unchecked path open.
	if !sameOrigin(u.Host, u.Scheme, r.Host, requestScheme(r, cfg)) {
		return true
	}
	return isSecFetchSiteCrossOrigin(r.Header.Get("Sec-Fetch-Site"))
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

// levelForStatus maps an HTTP status code to the log level its request
// summary line should be emitted at: 5xx is a server-side failure (Error),
// 4xx is a client-rejected request worth flagging (Warn), everything else
// is routine (Info).
func levelForStatus(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// loggingMiddleware records exactly one line per request, at a level
// derived from the final status code, with method, path, status, duration,
// source IP, and (for non-2xx responses) the error message set via
// respondError. This is the single point of request logging for the API —
// respondError does not log separately, to avoid a duplicate line per
// failed request (one from the handler, one from this middleware).
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
			"remote_ip", remoteIP(r),
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
		if sw.errMsg != "" {
			attrs = append(attrs, "error", sw.errMsg)
		}
		//nolint:gosec // G706: slog structured fields are not vulnerable to log injection
		s.log.Log(r.Context(), levelForStatus(sw.status), "api", attrs...)
	})
}

// secretParamSubstrings are lowercase substrings that mark a query
// parameter *name* as secret-bearing. Matched via strings.Contains so
// api_key, secret_token, etc. are all caught, not just exact names.
var secretParamSubstrings = []string{"pass", "key", "secret", "token"}

// sanitizeQuery redacts secret-bearing query values so they don't leak into
// logs. It redacts by two rules: (1) any parameter name containing a
// secret-like substring (apikey, nzbkey, password, secret, token, ...), and
// (2) the "value" parameter when a sibling "keyword" parameter names a known
// secret field (mode=set_config&keyword=password&value=...,
// keyword=api_key&value=..., keyword=nzb_key&value=...). The keyword check
// reuses isSecretParamName so it stays in sync with the config's actual
// secret-bearing fields (password, api_key, nzb_key — see
// internal/config/general.go, servers.go, notifications.go) rather than
// requiring a separately maintained list. Uses url.ParseQuery to handle
// URL-encoded parameter names (e.g. %61pikey → apikey) that would bypass a
// raw string prefix check.
func sanitizeQuery(raw string) string {
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		// Unparseable query — redact entirely to be safe.
		return "***"
	}
	// Capture the keyword value before the redaction loop below, since the
	// param name "keyword" itself contains the secret-like substring "key"
	// and would otherwise be redacted before we get a chance to inspect it.
	keyword := strings.ToLower(parsed.Get("keyword"))
	for key := range parsed {
		lower := strings.ToLower(key)
		if isSecretParamName(lower) {
			parsed.Set(key, "***")
		}
	}
	if isSecretParamName(keyword) && parsed.Has("value") {
		parsed.Set("value", "***")
	}
	return parsed.Encode()
}

// isSecretParamName reports whether a query parameter name (already
// lowercased) is likely to carry a credential.
func isSecretParamName(lower string) bool {
	for _, sub := range secretParamSubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

// statusWriter wraps ResponseWriter to capture the status code for logging.
// errMsg is set by respondError so loggingMiddleware can fold the
// human-readable error into its single per-request log line instead of
// respondError logging a separate one.
type statusWriter struct {
	http.ResponseWriter
	status      int
	errMsg      string
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
