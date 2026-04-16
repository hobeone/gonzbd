// Package web serves the SABnzbd-Go web interface and static Glitter
// assets. Static assets are embedded at build time via //go:embed so
// the binary is self-contained.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler returns an http.Handler serving the web UI and static assets.
//
// Routes:
//   - GET /              → the placeholder index.html (sub-filesystem of static/)
//   - GET /static/...    → Glitter assets under static/glitter/
//   - GET /staticcfg/... → shared icons (favicons, apple-touch-icons) under static/staticcfg/.
//     Mounted at /staticcfg/ rather than /static/staticcfg/ because upstream
//     SABnzbd's main.tmpl references these via "./staticcfg/ico/..." paths.
//
// The handler is stateless and safe to serve concurrently.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// This can only happen with a broken build — the embed directive
		// guarantees "static" exists at compile time.
		panic("web: embed subtree 'static' missing: " + err.Error())
	}

	staticcfgSub, err := fs.Sub(staticFS, "static/staticcfg")
	if err != nil {
		panic("web: embed subtree 'static/staticcfg' missing: " + err.Error())
	}

	mux := http.NewServeMux()

	// Serve /static/ prefix — strip the prefix then serve from the embed FS.
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// Serve /staticcfg/ prefix from the staticcfg subtree (matches upstream URL layout).
	mux.Handle("/staticcfg/", http.StripPrefix("/staticcfg/", http.FileServer(http.FS(staticcfgSub))))

	// Serve index.html at "/".
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "index not found", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data) //nolint:errcheck // write to ResponseWriter; error unrecoverable
	})

	return mux
}
