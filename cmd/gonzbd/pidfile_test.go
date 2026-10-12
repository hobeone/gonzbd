package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestWritePIDFile_WritesPIDUnderNewParent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "run", "gonzbd.pid")
	if err := writePIDFile(path); err != nil {
		t.Fatalf("writePIDFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(os.Getpid()) + "\n"; string(got) != want {
		t.Errorf("pid file = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("parent holds %d entries, want only the pid file", len(entries))
	}
}

func TestWritePIDFile_ParentIsAFile(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "run")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePIDFile(filepath.Join(parent, "gonzbd.pid")); err == nil {
		t.Error("writePIDFile succeeded under a parent that is a regular file")
	}
}
