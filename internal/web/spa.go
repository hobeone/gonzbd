package web

import (
	"io/fs"
	"net/http"
)

// NewSPAHandler returns an http.Handler that serves a Vite-built SPA.
// The dist parameter should be rooted at the build output directory
// (index.html at the root, hashed assets under _app/).
//
// Static files that exist in the FS are served directly. Any path that
// does not match a real file is served as index.html so that the SPA's
// client-side router can handle it.
//
// When the root "/" is requested, it sets a "sab_apikey" cookie so the
// client-side JS can hit the /api without needing an explicit key.
func NewSPAHandler(dist fs.FS, apiKey string) http.Handler {
	fileServer := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			http.SetCookie(w, &http.Cookie{
				Name:     "sab_apikey",
				Value:    apiKey,
				Path:     "/",
				HttpOnly: false, // JS might need to read it (though backend uses it)
				SameSite: http.SameSiteLaxMode,
			})
			fileServer.ServeHTTP(w, r)
			return
		}

		// Strip leading slash for fs.Stat lookup.
		clean := path[1:]
		if _, err := fs.Stat(dist, clean); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA catch-all: serve index.html for client-side routing.
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
