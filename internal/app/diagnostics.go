package app

import (
	"os/exec"
)

// CheckDependencies verifies that the required external programs (unrar, 7z, par2)
// are available in the system PATH. Returns a list of warning messages for
// any missing dependencies.
func CheckDependencies() []string {
	var warnings []string

	// par2 is required for repair.
	if _, err := exec.LookPath("par2"); err != nil {
		warnings = append(warnings, "External program \"par2\" not found in PATH. PAR2 repair will fail.")
	}

	// 7-zip: prefer 7zz, fall back to 7z.
	has7zip := false
	if _, err := exec.LookPath("7zz"); err == nil {
		has7zip = true
	} else if _, err := exec.LookPath("7z"); err == nil {
		has7zip = true
	}

	// unrar is optional — 7zip handles RAR3/4/5 natively.
	hasUnrar := false
	if _, err := exec.LookPath("unrar"); err == nil {
		hasUnrar = true
	}

	if !has7zip && !hasUnrar {
		warnings = append(warnings, "Neither \"7-zip\" nor \"unrar\" found in PATH. Archive extraction will fail.")
	} else if !has7zip {
		warnings = append(warnings, "External program \"7-zip\" not found in PATH. 7z/zip extraction will fail. RAR extraction will use unrar.")
	} else if !hasUnrar {
		// Not a warning — 7zip covers RAR extraction as a fallback.
		_ = hasUnrar
	}

	return warnings
}
