package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeDocs materialises a docs/reviews-shaped directory.
func writeDocs(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const goodBanner = "# Lane\n\n> **Frozen record.** Snapshot of `main` @ `9be19e24`, not a living contract.\n"

// TestCheck_AcceptsACompleteBanner is the control. Without it a bug that
// reported every file would still make the two negative cases below pass.
func TestCheck_AcceptsACompleteBanner(t *testing.T) {
	got, err := check(writeDocs(t, map[string]string{"lane1.md": goodBanner}))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("check reported %v, want no findings", got)
	}
}

func TestCheck_FlagsEachMissingHalfSeparately(t *testing.T) {
	dir := writeDocs(t, map[string]string{
		// Has the commit, lacks the label.
		"nolabel.md": "# Lane\n\n> Snapshot of `main` @ `9be19e24`.\n",
		// Has the label, lacks any commit — the unfalsifiable shape.
		"nocommit.md": "# Lane\n\n> **Frozen record.** Not a living contract.\n",
	})
	got, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("check reported %d findings, want 2: %v", len(got), got)
	}
	// Sorted by path: nocommit.md before nolabel.md.
	if n := len(got[0].markers); n != 1 || got[0].path != filepath.Join(dir, "nocommit.md") {
		t.Errorf("finding 0 = %+v, want nocommit.md missing exactly one marker", got[0])
	}
	if n := len(got[1].markers); n != 1 || got[1].path != filepath.Join(dir, "nolabel.md") {
		t.Errorf("finding 1 = %+v, want nolabel.md missing exactly one marker", got[1])
	}
}

// TestCheck_IgnoresNonMarkdown guards against the gate firing on a stray
// asset dropped into docs/reviews/.
func TestCheck_IgnoresNonMarkdown(t *testing.T) {
	got, err := check(writeDocs(t, map[string]string{
		"lane1.md":  goodBanner,
		"notes.txt": "no banner here",
	}))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("check reported %v, want no findings", got)
	}
}

// TestCheck_MissingDirectoryIsAnError pins the choice documented on check: a
// vanished directory must not read as a pass, because that silently disables
// the gate.
func TestCheck_MissingDirectoryIsAnError(t *testing.T) {
	if _, err := check(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("check on a missing directory returned nil error, want an error")
	}
}

// TestCheck_RejectsATooShortHash pins that the commit half is a real pattern
// and not merely "some backticks are present". `main` alone must not satisfy
// it, or every file with an inline code span would pass the commit half.
func TestCheck_RejectsATooShortHash(t *testing.T) {
	got, err := check(writeDocs(t, map[string]string{
		"lane1.md": "# Lane\n\n> **Frozen record.** Snapshot of `main`, not a living contract.\n",
	}))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("check reported %d findings, want 1 (the commit half unsatisfied)", len(got))
	}
}

func TestCheckDir_SkipsOnlyWhenAllowMissingIsTrue(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")

	// When allowMissing is true (default -dir when docs/reviews is absent),
	// a missing directory returns skipped=true and nil error.
	missing, skipped, err := checkDir(absent, true)
	if err != nil {
		t.Fatalf("checkDir(absent, true) unexpected error: %v", err)
	}
	if !skipped {
		t.Error("checkDir(absent, true) skipped = false, want true")
	}
	if len(missing) != 0 {
		t.Errorf("checkDir(absent, true) findings = %v, want empty", missing)
	}

	// When allowMissing is false (explicit -dir), a missing directory returns an error.
	if _, skipped, err := checkDir(absent, false); err == nil || skipped {
		t.Errorf("checkDir(absent, false) = (skipped=%v, err=%v), want (false, non-nil error)", skipped, err)
	}

	// When the directory exists, allowMissing=true still checks its contents and reports findings.
	dir := writeDocs(t, map[string]string{
		"bad.md": "# No banner here\n",
	})
	missing, skipped, err = checkDir(dir, true)
	if err != nil {
		t.Fatalf("checkDir(existing, true) unexpected error: %v", err)
	}
	if skipped {
		t.Error("checkDir(existing, true) skipped = true, want false")
	}
	if len(missing) != 1 {
		t.Errorf("checkDir(existing, true) findings = %d, want 1", len(missing))
	}
}
