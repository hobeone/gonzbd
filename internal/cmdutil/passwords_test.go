package cmdutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPasswordFile_EmptyPath(t *testing.T) {
	pws, err := LoadPasswordFile("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pws != nil {
		t.Errorf("expected nil for empty path, got %v", pws)
	}
}

func TestLoadPasswordFile_Basic(t *testing.T) {
	f := filepath.Join(t.TempDir(), "passwords.txt")
	content := "secret1\n\n# comment\nsecret2\n  secret3  \n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	pws, err := LoadPasswordFile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"secret1", "secret2", "secret3"}
	if len(pws) != len(want) {
		t.Fatalf("got %d passwords, want %d", len(pws), len(want))
	}
	for i, w := range want {
		if pws[i] != w {
			t.Errorf("password[%d] = %q, want %q", i, pws[i], w)
		}
	}
}

func TestLoadPasswordFile_NonExistent(t *testing.T) {
	_, err := LoadPasswordFile("/nonexistent/passwords.txt")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoadPasswordFile_AllComments(t *testing.T) {
	f := filepath.Join(t.TempDir(), "passwords.txt")
	content := "# comment only\n# another\n\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	pws, err := LoadPasswordFile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pws) != 0 {
		t.Errorf("expected 0 passwords, got %d", len(pws))
	}
}
