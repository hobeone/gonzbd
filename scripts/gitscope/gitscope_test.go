package gitscope

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newRepo builds a throwaway git repo with one commit on main and chdirs into
// it for the duration of the test. gitscope shells out to git against the
// process working directory, so the tests need a real repo rather than a fake.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")

	writeFile(t, dir, "committed.go", "package p\n\nfunc A() {}\n")
	run("add", ".")
	run("commit", "-q", "-m", "initial")

	t.Chdir(dir)
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// The whole point of the package: work that is not yet committed must still
// be in scope. Each of these was invisible to the gates before, which made a
// pre-commit run report a pass that had inspected nothing.
func TestFiles_IncludesUncommittedWork(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{
			name: "unstaged modification",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "committed.go", "package p\n\nfunc A() { _ = 1 }\n")
			},
			want: "committed.go",
		},
		{
			name: "staged modification",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "committed.go", "package p\n\nfunc A() { _ = 2 }\n")
				gitIn(t, dir, "add", "committed.go")
			},
			want: "committed.go",
		},
		{
			name: "untracked new file",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "brand_new.go", "package p\n\nfunc B() {}\n")
			},
			want: "brand_new.go",
		},
		{
			name: "staged new file",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "staged_new.go", "package p\n\nfunc C() {}\n")
				gitIn(t, dir, "add", "staged_new.go")
			},
			want: "staged_new.go",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := newRepo(t)
			tc.setup(t, dir)

			files, err := Files()
			if err != nil {
				t.Fatalf("Files: %v", err)
			}
			if !slices.Contains(files, tc.want) {
				t.Errorf("Files() = %v, want it to contain %q", files, tc.want)
			}
		})
	}
}

// Ignored files must stay out of scope, or the gates would start reporting on
// build output and vendored trees.
func TestFiles_ExcludesIgnored(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, ".gitignore", "ignored.go\n")
	writeFile(t, dir, "ignored.go", "package p\n")

	files, err := Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if slices.Contains(files, "ignored.go") {
		t.Errorf("Files() = %v, want it to exclude the gitignored file", files)
	}
}

func TestFiles_CleanTreeYieldsNoUncommittedEntries(t *testing.T) {
	newRepo(t)

	files, err := Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	// origin/main does not exist here, so the base falls back to HEAD~1,
	// which also does not exist on a single-commit repo. Both degrade to an
	// empty set rather than erroring — the caller reports "nothing to check".
	if len(files) != 0 {
		t.Errorf("Files() = %v, want empty on a clean single-commit repo", files)
	}
}

// check_coverage needs line ranges, not just paths, and derives them by
// parsing "+++ b/<path>" headers. An untracked file has no diff of its own, so
// Diff synthesises one against /dev/null; this pins that the synthesised
// header is in the form that parser expects.
func TestDiff_RendersUntrackedFileWithParseableHeader(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "brand_new.go", "package p\n\nfunc B() {}\n")

	diff, err := Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if !strings.Contains(diff, "+++ b/brand_new.go") {
		t.Errorf("Diff() lacks a parseable +++ b/ header for the untracked file:\n%s", diff)
	}
	if !strings.Contains(diff, "@@ -0,0 +1") {
		t.Errorf("Diff() lacks a hunk header covering the new file's lines:\n%s", diff)
	}
}

func TestDiff_IncludesUnstagedHunks(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "committed.go", "package p\n\nfunc A() { _ = 1 }\n")

	diff, err := Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "+++ b/committed.go") {
		t.Errorf("Diff() does not cover the unstaged modification:\n%s", diff)
	}
}
