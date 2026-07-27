package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// NewSPAHandler returns an http.Handler that serves a Vite-built SPA.
// The dist parameter should be rooted at the build output directory
// (index.html at the root, hashed assets under _app/).
//
// Static files that exist in the FS are served directly. Any path without
// a file extension that does not match a real file is served as index.html
// so that the SPA's client-side router can handle it. Paths with extensions
// return 404.
//
// When the root "/" is requested, it sets a
// "gonzbd_apikey" cookie so the client-side JS can hit the /api without
// needing an explicit key. apiKeyFn should return the API server's
// ephemeral session key (Server.SessionKey), not the permanent
// General.APIKey — see AuthConfig.SessionKey for why these are kept
// distinct.
//
// The cookie is a full-admin credential, so it is only issued to trusted
// clients: trustedFn reports whether the requesting client (by source IP,
// per config.IsTrustedRemote) is loopback or within a configured
// local_range. Untrusted clients still receive the static SPA, but no
// session cookie — they must authenticate with the real API key. This
// closes SEC-1: an unauthenticated non-loopback caller can no longer
// obtain admin simply by requesting "/". The API auth middleware enforces
// the same trust gate when accepting the cookie, so a leaked cookie cannot
// be replayed from an untrusted address either.
func NewSPAHandler(dist fs.FS, apiKeyFn func() string, trustedFn func(*http.Request) bool) http.Handler {
	fileServer := http.FileServerFS(dist)

	setAPIKeyCookie := func(w http.ResponseWriter, req *http.Request) {
		if trustedFn != nil && !trustedFn(req) {
			return
		}
		secure := req.TLS != nil || strings.EqualFold(req.Header.Get("X-Forwarded-Proto"), "https")
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is set dynamically based on TLS, HttpOnly is true.
			Name:     "gonzbd_apikey",
			Value:    apiKeyFn(),
			Path:     "/",
			HttpOnly: true, // Protected against XSS; browser sends cookie header automatically.
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := r.URL.Path
		if reqPath == "/" {
			w.Header().Set("Cache-Control", "no-cache")
			setAPIKeyCookie(w, r)
			fileServer.ServeHTTP(w, r)
			return
		}

		// Strip leading slash for fs.Stat lookup.
		clean := strings.TrimPrefix(reqPath, "/")
		if _, err := fs.Stat(dist, clean); err == nil {
			// Known static asset — serve without auth check.
			// Vite-hashed assets (under /assets/) use content-hash
			// filenames and can be cached indefinitely. Other files
			// (e.g. favicon.ico) get a short cache.
			if strings.HasPrefix(clean, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// A missing path with a file extension is a dead asset reference
		// (404), not a client-side route -- SPA routes are always
		// extension-less paths. Check before authCheck since a 404 for a
		// non-existent asset shouldn't require authentication to learn.
		if path.Ext(clean) != "" {
			http.NotFound(w, r)
			return
		}

		// SPA catch-all: serve index.html for client-side routing.
		// Clone the request so the original path is preserved for
		// upstream logging middleware.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		setAPIKeyCookie(w, r)
		fileServer.ServeHTTP(w, r2)
	})
}
