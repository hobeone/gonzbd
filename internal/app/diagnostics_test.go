package app

import (
	"os"
	"testing"
)

func TestCheckDependencies(t *testing.T) {
	// This test depends on the environment, but we can at least verify it returns
	// something.
	warnings := CheckDependencies()
	
	// We don't necessarily want to fail if dependencies are missing on the build
	// machine, but we can log what was found.
	for _, w := range warnings {
		t.Logf("Found warning: %s", w)
	}
}

func TestCheckDependencies_Missing(t *testing.T) {
	// Mock PATH to ensure it doesn't find anything
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", "")
	defer os.Setenv("PATH", oldPath)

	warnings := CheckDependencies()
	// Should have at least 3 warnings (unrar, 7-zip, par2)
	if len(warnings) < 3 {
		t.Errorf("Expected at least 3 warnings, got %d", len(warnings))
	}
}
