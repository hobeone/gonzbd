// Package web serves the Svelte 5 SPA for the SABnzbd-Go web interface.
// Static assets are embedded at build time via //go:embed so the binary
// is self-contained.
package web

import (
	"io/fs"
	"net/http"

	"github.com/hobeone/sabnzbd-go/ui"
)

// Handler returns an http.Handler serving the Svelte 5 SPA from the
// Vite-built ui/dist directory embedded in the project.
//
// The handler is stateless and safe to serve concurrently.
func Handler() http.Handler {
	dist, err := fs.Sub(ui.DistFS, "dist")
	if err != nil {
		// Can only happen if the build process didn't run 'npm run build'
		// and the ui/dist folder is missing.
		panic("web: embedded ui/dist subtree missing — run 'cd ui && npm run build' first: " + err.Error())
	}
	return NewSPAHandler(dist)
}
