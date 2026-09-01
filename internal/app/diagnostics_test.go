package app

import (
	"errors"
	"testing"
)

func TestCheckDependencies(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	// Inject a lookup that finds nothing, rather than mutating the real PATH
	// env var — PATH is process-global, so mutating it would race with any
	// other parallel test resolving a binary through the real PATH.
	notFound := func(file string) (string, error) {
		return "", errors.New("not found")
	}

	warnings := checkDependencies(notFound)
	expected := []string{
		`External program "par2" not found in PATH. PAR2 repair will fail.`,
		`Neither "7-zip" nor "unrar" found in PATH. Archive extraction will fail.`,
	}
	if len(warnings) != len(expected) {
		t.Fatalf("Expected exactly %d warnings, got %d: %v", len(expected), len(warnings), warnings)
	}
	for i, w := range warnings {
		if w != expected[i] {
			t.Errorf("Warning at %d mismatch: expected %q, got %q", i, expected[i], w)
		}
	}
}
