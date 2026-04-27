package web

import (
	"io/fs"
	"net/http"
)

// AuthCheck is called before serving the SPA. It returns true if the
// request is authenticated (valid Basic Auth credentials, localhost
// bypass, or API key cookie). When it returns false, it must have
// written a 401 response.
type AuthCheck func(w http.ResponseWriter, r *http.Request) bool

// NewSPAHandler returns an http.Handler that serves a Vite-built SPA.
// The dist parameter should be rooted at the build output directory
// (index.html at the root, hashed assets under _app/).
//
// Static files that exist in the FS are served directly. Any path that
// does not match a real file is served as index.html so that the SPA's
// client-side router can handle it.
//
// When the root "/" is requested and auth succeeds, it sets a
// "sab_apikey" cookie so the client-side JS can hit the /api without
// needing an explicit key.
//
// authCheck is called on every navigation request (root and SPA
// catch-all). Static asset requests (JS/CSS/images) are served without
// auth so that browser caching works. If authCheck is nil, all requests
// are allowed (useful for tests).
func NewSPAHandler(dist fs.FS, apiKeyFn func() string, authCheck AuthCheck) http.Handler {
	fileServer := http.FileServerFS(dist)

	setApiKeyCookie := func(w http.ResponseWriter) {
		http.SetCookie(w, &http.Cookie{
			Name:     "sab_apikey",
			Value:    apiKeyFn(),
			Path:     "/",
			HttpOnly: false, // JS reads it to attach as X-API-Key header
			SameSite: http.SameSiteStrictMode,
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			if authCheck != nil && !authCheck(w, r) {
				return
			}
			setApiKeyCookie(w)
			fileServer.ServeHTTP(w, r)
			return
		}

		// Strip leading slash for fs.Stat lookup.
		clean := path[1:]
		if _, err := fs.Stat(dist, clean); err == nil {
			// Known static asset — serve without auth check.
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA catch-all: serve index.html for client-side routing.
		if authCheck != nil && !authCheck(w, r) {
			return
		}
		// Clone the request so the original path is preserved for
		// upstream logging middleware.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		setApiKeyCookie(w)
		fileServer.ServeHTTP(w, r2)
	})
}
