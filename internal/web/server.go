// Package web serves the Svelte 5 SPA for the SABnzbd-Go web interface.
// Static assets are embedded at build time via //go:embed so the binary
// is self-contained.
package web

import (
	"fmt"
	"io/fs"
	"net/http"

	"github.com/hobeone/sabnzbd-go/ui"
)

// Handler returns an http.Handler serving the Svelte 5 SPA from the
// Vite-built ui/dist directory embedded in the project.
//
// Returns an error if the embedded dist directory is missing (i.e. the
// UI was not built before compiling).
//
// The handler is stateless and safe to serve concurrently.
func Handler(apiKey string) (http.Handler, error) {
	dist, err := fs.Sub(ui.DistFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("web: embedded ui/dist subtree missing — run 'cd ui && npm run build' first: %w", err)
	}
	return NewSPAHandler(dist, apiKey), nil
}
