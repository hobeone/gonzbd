package unpack

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// UnrarInfo describes the installed unrar binary, detected via `unrar` output.
type UnrarInfo struct {
	// Version is the parsed version as an integer (e.g. 550 for 5.50, 721 for 7.21).
	Version int
	// VersionStr is the raw version string (e.g. "5.50").
	VersionStr string
	// HasProblem is true when the binary is too old (< 5.50) or its version
	// could not be determined. In this mode, flags that old/non-RARLAB unrar
	// variants don't support are stripped: -scf, -or, -ai, -tsm-.
	// Matches SABnzbd's RAR_PROBLEM degraded mode.
	HasProblem bool
	// Available is true when the binary was found on PATH.
	Available bool
}

// SevenzInfo describes the installed 7z binary.
type SevenzInfo struct {
	// Version is the version string (e.g. "21.06").
	Version string
	// Available is true when the binary was found on PATH.
	Available bool
}

var unrarVersionRE = regexp.MustCompile(`(?i)UNRAR\s+(\d+)\.(\d+)`)

// parseUnrarOutput extracts the version from unrar's stdout/stderr.
func parseUnrarOutput(output string) UnrarInfo {
	info := UnrarInfo{Available: true}

	// Parse version: "UNRAR 5.50 freeware" or "UNRAR 7.21 freeware"
	if m := unrarVersionRE.FindStringSubmatch(output); len(m) == 3 {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		info.Version = major*100 + minor
		info.VersionStr = m[1] + "." + m[2]
	}

	// Version < 5.50 (or unparseable) means degraded mode: omit flags that
	// old/non-RARLAB variants don't support.
	info.HasProblem = info.Version < 550

	return info
}

// DetectUnrar probes the unrar binary to determine version and authenticity.
// Returns zero-value UnrarInfo if the binary is not available.
func DetectUnrar(ctx context.Context, bin string) UnrarInfo {
	if bin == "" {
		bin = "unrar"
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin).CombinedOutput() //nolint:gosec // binary path from config or SevenZipBinaries constant list
	if err != nil && len(out) == 0 {
		// Binary not found or can't execute.
		return UnrarInfo{}
	}

	return parseUnrarOutput(string(out))
}

var sevenzVersionRE = regexp.MustCompile(`(?i)7-Zip\s.*?(\d+\.\d+)`)

// DetectSevenZip probes the 7z binary to determine its version.
// Tries multiple binary names in the same order as sevenZipBin: 7zz, 7zzs, 7z, 7za.
func DetectSevenZip(ctx context.Context, bin string) SevenzInfo {
	candidates := []string{bin}
	if bin == "" {
		candidates = SevenZipBinaries
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for _, cand := range candidates {
		out, err := exec.CommandContext(ctx, cand).CombinedOutput() //nolint:gosec // binary path from config or SevenZipBinaries constant list
		if err != nil && len(out) == 0 {
			continue
		}
		output := string(out)
		info := SevenzInfo{Available: true}

		if m := sevenzVersionRE.FindStringSubmatch(output); len(m) == 2 {
			info.Version = m[1]
		}
		return info
	}

	return SevenzInfo{}
}
