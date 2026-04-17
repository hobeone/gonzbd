// Package ui embeds the Vite-built SPA assets for inclusion in the Go binary.
// Run "cd ui && npm run build" before "go build" to populate the dist/ directory.
package ui

import "embed"

// DistFS holds the Vite build output (ui/dist/) embedded at compile time.
//
//go:embed all:dist
var DistFS embed.FS
