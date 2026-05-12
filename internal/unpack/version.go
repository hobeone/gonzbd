package unpack

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// UnrarInfo describes the installed unrar binary, detected via `unrar` output.
// SABnzbd checks for Alexander Roshal's original binary and version >= 5.50.
type UnrarInfo struct {
	// Version is the parsed version as an integer (e.g. 550 for 5.50, 710 for 7.10).
	Version int
	// VersionStr is the raw version string (e.g. "5.50").
	VersionStr string
	// Original is true when the binary contains "Alexander L. Roshal"
	// (the original unrar, not a fork like unar or bsdtar).
	Original bool
	// HasProblem is true when the binary is non-original or too old (< 5.50).
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

// DetectUnrar probes the unrar binary to determine version and authenticity.
// Returns zero-value UnrarInfo if the binary is not available.
func DetectUnrar(ctx context.Context, bin string) UnrarInfo {
	if bin == "" {
		bin = "unrar"
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin).CombinedOutput() //nolint:gosec
	if err != nil && len(out) == 0 {
		// Binary not found or can't execute.
		return UnrarInfo{}
	}
	output := string(out)

	info := UnrarInfo{Available: true}

	// Check for original unrar (Alexander L. Roshal).
	if strings.Contains(output, "Alexander L. Roshal") {
		info.Original = true
	}

	// Parse version: "UNRAR 5.50 freeware" or "UNRAR 7.10 x64"
	if m := unrarVersionRE.FindStringSubmatch(output); len(m) == 3 {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		info.Version = major*100 + minor
		info.VersionStr = m[1] + "." + m[2]
	}

	// Flag problems: non-original or version < 5.50.
	if !info.Original || info.Version < 550 {
		info.HasProblem = true
	}

	return info
}

var sevenzVersionRE = regexp.MustCompile(`(?i)7-Zip\s.*?(\d+\.\d+)`)

// DetectSevenZip probes the 7z binary to determine its version.
// Tries multiple binary names in order: 7z, 7zzs, 7za.
func DetectSevenZip(ctx context.Context, bin string) SevenzInfo {
	candidates := []string{bin}
	if bin == "" {
		candidates = []string{"7z", "7zzs", "7za"}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for _, cand := range candidates {
		out, err := exec.CommandContext(ctx, cand).CombinedOutput() //nolint:gosec
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
