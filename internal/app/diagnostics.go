package app

import (
	"fmt"
	"os/exec"
)

// CheckDependencies verifies that the required external programs (unrar, 7z, par2)
// are available in the system PATH. Returns a list of warning messages for
// any missing dependencies.
func CheckDependencies() []string {
	var warnings []string

	deps := []struct {
		name string
		bin  string
	}{
		{name: "unrar", bin: "unrar"},
		{name: "7-zip", bin: "7zz"}, // Try 7zz first (preferred)
		{name: "par2", bin: "par2"},
	}

	for _, d := range deps {
		if _, err := exec.LookPath(d.bin); err != nil {
			if d.name == "7-zip" {
				// Fallback to 7z
				if _, err := exec.LookPath("7z"); err == nil {
					continue
				}
			}
			warnings = append(warnings, fmt.Sprintf("External program %q not found in PATH. Post-processing may fail.", d.name))
		}
	}

	return warnings
}
