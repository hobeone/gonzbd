package app

import (
	"os/exec"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// CheckDependencies verifies that the required external programs (unrar, 7z, par2)
// are available in the system PATH. Returns a list of warning messages for
// any missing dependencies.
func CheckDependencies() []string {
	return checkDependencies(exec.LookPath)
}

// checkDependencies is CheckDependencies with the binary lookup injected, so
// tests can simulate missing binaries via a closure instead of mutating the
// process-wide PATH environment variable (which would race with any other
// parallel test that resolves a binary through the real PATH).
func checkDependencies(lookPath func(file string) (string, error)) []string {
	var warnings []string

	// par2 is required for repair.
	if _, err := lookPath("par2"); err != nil {
		warnings = append(warnings, "External program \"par2\" not found in PATH. PAR2 repair will fail.")
	}

	// 7-zip: check all known binary names (7zz, 7zzs, 7z, 7za).
	has7zip := false
	for _, name := range unpack.SevenZipBinaries {
		if _, err := lookPath(name); err == nil {
			has7zip = true
			break
		}
	}

	// unrar is optional — 7zip handles RAR3/4/5 natively.
	hasUnrar := false
	if _, err := lookPath("unrar"); err == nil {
		hasUnrar = true
	}

	switch {
	case !has7zip && !hasUnrar:
		warnings = append(warnings, "Neither \"7-zip\" nor \"unrar\" found in PATH. Archive extraction will fail.")
	case !has7zip:
		warnings = append(warnings, "External program \"7-zip\" not found in PATH. 7z/zip extraction will fail. RAR extraction will use unrar.")
	case !hasUnrar:
		// Not a warning — 7zip covers RAR extraction as a fallback.
	}

	return warnings
}
