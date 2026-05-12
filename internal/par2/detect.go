package par2

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Par2Caps describes the capabilities of the installed par2 binary,
// detected at startup via `par2 -h` / `par2 -V` output parsing.
// SABnzbd uses similar detection in newsunpack.py to conditionally
// add -N and -B flags.
type Par2Caps struct {
	// SupportsNoDataSkipping is true when par2 supports -N (no data
	// skipping during verification). This speeds up verification
	// significantly for undamaged files.
	SupportsNoDataSkipping bool

	// SupportsBasepath is true when par2 supports -B <dir>, which
	// tells it where to find the data files. Critical for par2
	// builds that don't default to searching the par2 file's dir.
	SupportsBasepath bool

	// IsTurbo is true when the par2 binary is par2cmdline-turbo,
	// which supports -t+ for multi-threaded operation.
	IsTurbo bool

	// Version is the version string from the par2 binary, or empty
	// if detection failed.
	Version string
}

// DetectCapabilities probes the par2 binary's capabilities by running
// `par2 -h` and parsing the help output. The ctx should have a short
// timeout since this runs at startup.
//
// Returns an empty Par2Caps (not nil) on failure — the caller can
// safely use zero-value caps.
func DetectCapabilities(ctx context.Context, par2Bin string) Par2Caps {
	if par2Bin == "" {
		par2Bin = "par2"
	}

	// Use a short timeout for the probe.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Run par2 -h (or --help) — different builds use different flags
	// but -h is most universal. par2 -h returns exit code 1 on most
	// builds, so we ignore exit errors.
	out, _ := exec.CommandContext(ctx, par2Bin, "-h").CombinedOutput() //nolint:gosec // binary path from config
	help := string(out)

	// Also try -V for version detection.
	vOut, _ := exec.CommandContext(ctx, par2Bin, "-V").CombinedOutput() //nolint:gosec
	versionStr := strings.TrimSpace(string(vOut))

	var caps Par2Caps

	// Detect -N support (no data skipping).
	// par2cmdline-turbo help says "No data skipping" or shows "-N"
	// Original par2cmdline shows "-N  : No data skipping"
	if strings.Contains(help, "No data skipping") ||
		strings.Contains(help, "-N ") {
		caps.SupportsNoDataSkipping = true
	}

	// Detect -B (basepath) support.
	// par2cmdline and par2cmdline-turbo show "Set the basepath" or "-B <path>"
	if strings.Contains(help, "Set the basepath") ||
		strings.Contains(help, "-B ") ||
		strings.Contains(help, "-B<") {
		caps.SupportsBasepath = true
	}

	// Detect turbo (par2cmdline-turbo).
	lower := strings.ToLower(help + " " + versionStr)
	if strings.Contains(lower, "turbo") ||
		strings.Contains(lower, "-t+") {
		caps.IsTurbo = true
	}

	// Extract version from -V output.
	// Typical: "par2cmdline version 0.8.1"
	if idx := strings.Index(strings.ToLower(versionStr), "version"); idx >= 0 {
		caps.Version = strings.TrimSpace(versionStr[idx+len("version"):])
		// Take only the first line.
		if nl := strings.IndexByte(caps.Version, '\n'); nl >= 0 {
			caps.Version = caps.Version[:nl]
		}
	} else if versionStr != "" && len(versionStr) < 200 {
		caps.Version = versionStr
	}

	return caps
}
