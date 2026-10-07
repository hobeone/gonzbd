package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestChangedSince_CountsCommittedStagedUnstagedAndUntracked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "test")
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go"} {
		mustWrite(t, filepath.Join(dir, f), "seed\n")
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "seed")
	ref := gitIn(t, dir, "rev-parse", "HEAD")

	mustWrite(t, filepath.Join(dir, "a.go"), "committed\n")
	gitIn(t, dir, "commit", "-q", "-am", "a")
	mustWrite(t, filepath.Join(dir, "b.go"), "staged\n")
	gitIn(t, dir, "add", "b.go")
	mustWrite(t, filepath.Join(dir, "c.go"), "unstaged\n")
	mustWrite(t, filepath.Join(dir, "e.go"), "untracked\n")

	got, err := changedSince(dir, ref)
	if err != nil {
		t.Fatalf("changedSince: %v", err)
	}
	want := []string{"a.go", "b.go", "c.go", "e.go"}
	if !slices.Equal(got, want) {
		t.Errorf("changedSince = %v, want %v (d.go is untouched)", got, want)
	}
}

func TestChangedSince_AnUnknownRefIsAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	if _, err := changedSince(dir, "no-such-ref"); err == nil {
		t.Fatal("changedSince accepted a ref that does not exist")
	}
}

func TestChangedSince_ListsBothSidesOfARename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "test")
	mustWrite(t, filepath.Join(dir, "old.go"), "package p\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "seed")
	gitIn(t, dir, "mv", "old.go", "new.go")

	got, err := changedSince(dir, "HEAD")
	if err != nil {
		t.Fatalf("changedSince: %v", err)
	}
	if want := []string{"new.go", "old.go"}; !slices.Equal(got, want) {
		t.Errorf("changedSince = %v, want %v: a spec may mutate the old path", got, want)
	}
}

func TestChangedSince_ARefThatLooksLikeAnOptionIsNotRunAsOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	out := filepath.Join(dir, "written")
	if _, err := changedSince(dir, "--output="+out); err == nil {
		t.Fatal("changedSince accepted an option as a ref")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("git ran with the ref as an option and wrote a file")
	}
}

func TestAffectedSpecs_SelectsOnTheSpecOrAMutatedFileAndNothingElse(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "specs"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := func(file string) string {
		return "pkg ./p/\n[m]\nfile " + file + "\n--- anchor\nx\n--- replace\ny\n--- end\n"
	}
	mustWrite(t, filepath.Join(root, "specs/mutates_x.spec"), body("x.go"))
	mustWrite(t, filepath.Join(root, "specs/mutates_y.spec"), body("y.go"))
	mustWrite(t, filepath.Join(root, "specs/mutates_z.spec"), body("z.go"))
	specs := []string{"specs/mutates_x.spec", "specs/mutates_y.spec", "specs/mutates_z.spec"}

	cases := []struct {
		name    string
		changed []string
		want    []string
	}{
		{"a mutated file", []string{"x.go"}, []string{"specs/mutates_x.spec"}},
		{"the spec itself", []string{"specs/mutates_y.spec"}, []string{"specs/mutates_y.spec"}},
		{"both", []string{"x.go", "specs/mutates_y.spec"}, []string{"specs/mutates_x.spec", "specs/mutates_y.spec"}},
		{"a test file no spec mutates", []string{"x_test.go"}, nil},
		{"a file no spec names", []string{"other.go"}, nil},
		{"nothing", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := affectedSpecs(root, specs, tc.changed)
			if err != nil {
				t.Fatalf("affectedSpecs: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("affectedSpecs(%v) = %v, want %v", tc.changed, got, tc.want)
			}
		})
	}
}

func TestAffectedSpecs_AnUnreadableSpecIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := affectedSpecs(t.TempDir(), []string{"specs/missing.spec"}, []string{"x.go"}); err == nil {
		t.Fatal("affectedSpecs skipped a spec it could not read")
	}
}
