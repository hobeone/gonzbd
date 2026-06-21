// Package web serves the Svelte 5 SPA for the GoNZBD web interface.
// Static assets are embedded at build time via //go:embed so the binary
// is self-contained.
package web

import (
	"io/fs"
	"net/http"

	"github.com/hobeone/gonzbd/ui"
)

// Handler returns an http.Handler serving the Svelte 5 SPA from the
// Vite-built ui/dist directory embedded in the project.
//
// Returns an error if the embedded dist directory is missing (i.e. the
// UI was not built before compiling).
//
// The handler is stateless and safe to serve concurrently.
func Handler(apiKeyFn func() string) (http.Handler, error) {
	dist, _ := fs.Sub(ui.DistFS, "dist")
	return NewSPAHandler(dist, apiKeyFn), nil
}
