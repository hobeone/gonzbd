// Package humanfmt provides human-readable formatting for byte counts and
// durations, matching the output format of SABnzbd's Python implementation.
package humanfmt

import (
	"fmt"
	"time"
)

// Bytes formats a byte count using binary (power-of-1024) divisors with
// SABnzbd-compatible suffixes: "B", "KB", "MB", "GB", "TB".
//
//	Bytes(1536)        → "1.50 KB"
//	Bytes(1073741824)  → "1.00 GB"
func Bytes(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TB", float64(n)/float64(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// BytesSI formats bytes with 1-decimal precision and IEC-style suffixes:
// "KiB", "MiB", "GiB". Used for internal display (e.g. post-proc logs)
// where SABnzbd API compatibility is not required.
func BytesSI(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Duration renders a time.Duration as a human-readable string for internal
// display (post-proc logs, CLI output). Not for API output — use
// DurationSAB for the SABnzbd h:mm:ss format.
//
//	Duration(45 * time.Second)  → "45.0s"
//	Duration(5*time.Minute+30*time.Second) → "5m 30s"
//	Duration(2*time.Hour+15*time.Minute)   → "2h 15m"
func Duration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := m / 60
	m %= 60
	return fmt.Sprintf("%dh %dm", h, m)
}
