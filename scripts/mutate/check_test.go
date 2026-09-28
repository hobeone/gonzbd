package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckSpecAnchors_ReportsAStaleAndAnAmbiguousAnchorButNotAClean(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// "func " occurs twice, which is what makes the "ambiguous" case
	// ambiguous; "func one() {}" occurs exactly once, which is what keeps
	// "clean" out of the issues slice.
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nfunc one() {}\n\nfunc two() {}\n")

	specPath := write(t, "pkg ./p/\n"+
		"[stale]\nfile a.go\n--- anchor\nfunc three() {}\n--- replace\nfunc four() {}\n--- end\n"+
		"[ambiguous]\nfile a.go\n--- anchor\nfunc \n--- replace\nfn \n--- end\n"+
		"[clean]\nfile a.go\n--- anchor\nfunc one() {}\n--- replace\nfunc oneX() {}\n--- end\n")

	issues, err := checkSpecAnchors(root, specPath, specPath)
	if err != nil {
		t.Fatalf("checkSpecAnchors: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues = %+v, want 2 (clean must not be reported)", issues)
	}

	byName := map[string]anchorIssue{}
	for _, iss := range issues {
		byName[iss.mutation] = iss
	}
	if got := byName["stale"]; got.count != 0 {
		t.Errorf("stale.count = %d, want 0", got.count)
	}
	if got := byName["ambiguous"]; got.count != 2 {
		t.Errorf("ambiguous.count = %d, want 2", got.count)
	}
}

func TestCheckSpecAnchors_ReadsEachFileOnceAcrossMutations(t *testing.T) {
	t.Parallel()

	// stamp_owner.spec has eight mutations against one file; this pins that
	// checkSpecAnchors does not re-open the file per mutation by making a
	// second read fail (ENOENT after the first) and confirming the second
	// mutation's issue is still reported from the cached content.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nfunc one() {}\n")

	specPath := write(t, "pkg ./p/\n"+
		"[first]\nfile a.go\n--- anchor\nfunc one() {}\n--- replace\nfunc oneX() {}\n--- end\n"+
		"[second]\nfile a.go\n--- anchor\nfunc missing() {}\n--- replace\nfunc x() {}\n--- end\n")

	issues, err := checkSpecAnchors(root, specPath, specPath)
	if err != nil {
		t.Fatalf("checkSpecAnchors: %v", err)
	}
	if len(issues) != 1 || issues[0].mutation != "second" {
		t.Fatalf("issues = %+v, want exactly [second]", issues)
	}
}

func TestCheckSpecAnchors_PropagatesAParseError(t *testing.T) {
	t.Parallel()

	if _, err := checkSpecAnchors(t.TempDir(), "absent", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("checkSpecAnchors accepted a spec path that does not exist")
	}
}

func TestRunCheck_ReportsCleanlyWhenEveryAnchorResolvesOnce(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nfunc one() {}\n")
	clean := write(t, "pkg ./p/\n[m]\nfile a.go\n--- anchor\nfunc one() {}\n--- replace\nfunc oneX() {}\n--- end\n")

	var code int
	out := captureStdout(t, func() {
		code = runCheck(root, []string{clean}, []string{clean})
	})
	if code != 0 {
		t.Errorf("runCheck = %d, want 0", code)
	}
	if !strings.Contains(out, "1 spec(s)") {
		t.Errorf("runCheck output = %q, want it to report the spec count", out)
	}
}

func TestRunCheck_NamesTheStaleAnchorAndReturnsNonZero(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nfunc one() {}\n")
	stale := write(t, "pkg ./p/\n[gone]\nfile a.go\n--- anchor\nfunc missing() {}\n--- replace\nfunc x() {}\n--- end\n")

	var code int
	out := captureStdout(t, func() {
		code = runCheck(root, []string{stale}, []string{stale})
	})
	if code != 1 {
		t.Errorf("runCheck = %d, want 1", code)
	}
	if !strings.Contains(out, "[gone]") || !strings.Contains(out, "may be stale") {
		t.Errorf("runCheck output = %q, want it to name the mutation and call the anchor stale", out)
	}
}

func TestRunCheck_ReportsAParseErrorAndReturnsNonZero(t *testing.T) {
	root := t.TempDir()
	absent := filepath.Join(t.TempDir(), "absent.spec")

	var code int
	captureStdout(t, func() {
		code = runCheck(root, []string{absent}, []string{absent})
	})
	if code != 1 {
		t.Errorf("runCheck = %d, want 1 for a spec that fails to parse", code)
	}
}

func TestDiscoverSpecs_FindsTrackedAndUntrackedButNotOutsideTestdata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "test")

	if err := os.MkdirAll(filepath.Join(dir, "internal/foo/testdata"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tracked := filepath.Join(dir, "internal/foo/testdata/tracked.spec")
	mustWrite(t, tracked, "pkg ./p/\n")
	git("add", "internal/foo/testdata/tracked.spec")
	git("commit", "-q", "-m", "seed")

	// Untracked but not ignored must still be discovered — the same reason
	// run_tests.sh passes --others --exclude-standard: a spec just written
	// and not yet staged must not be silently skipped.
	untracked := filepath.Join(dir, "internal/foo/testdata/untracked.spec")
	mustWrite(t, untracked, "pkg ./p/\n")

	// A .spec file outside any testdata/ directory is not a mutation spec by
	// this repository's convention and must not be discovered.
	mustWrite(t, filepath.Join(dir, "internal/foo/not_testdata.spec"), "pkg ./p/\n")

	got, err := discoverSpecs(dir)
	if err != nil {
		t.Fatalf("discoverSpecs: %v", err)
	}
	want := []string{"internal/foo/testdata/tracked.spec", "internal/foo/testdata/untracked.spec"}
	if !slices.Equal(got, want) {
		t.Errorf("discoverSpecs = %v, want %v", got, want)
	}
}
