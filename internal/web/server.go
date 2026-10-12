// Package web serves the Svelte 5 SPA for the GoNZBD web interface.
// Static assets are embedded at build time via //go:embed so the binary
// is self-contained.
package web

import (
	"fmt"
	"io/fs"
	"net/http"

	"github.com/hobeone/gonzbd/ui"
)

// Handler returns an http.Handler serving the Svelte 5 SPA from the
// Vite-built ui/dist directory embedded in the project.
//
// Returns fs.Sub's error. embed.FS does not implement fs.SubFS, so fs.Sub
// fails on it only for an invalid path, which the constant "dist" is not; a
// missing ui/dist fails the build at //go:embed instead.
//
// trustedFn gates issuance of the admin session cookie to trusted clients
// only (see NewSPAHandler). Pass nil to always issue it (test-only).
//
// The handler is stateless and safe to serve concurrently.
func Handler(apiKeyFn func() string, trustedFn func(*http.Request) bool) (http.Handler, error) {
	return handlerFrom(ui.DistFS, apiKeyFn, trustedFn)
}

// handlerFrom serves the SPA from the dist directory of embedded.
func handlerFrom(embedded fs.FS, apiKeyFn func() string, trustedFn func(*http.Request) bool) (http.Handler, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, fmt.Errorf("web: embedded dist: %w", err)
	}
	return NewSPAHandler(dist, apiKeyFn, trustedFn), nil
}
