package gitscope

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

// changedLines parses hunk headers for one file out of a unified diff the
// same way check_coverage does, returning the new-side line numbers the diff
// claims changed. Kept deliberately close to that parser: the property under
// test is what a consumer of Diff() actually concludes, not what git printed.
func changedLines(t *testing.T, diff, file string) map[int]bool {
	t.Helper()
	lines := map[int]bool{}
	current := ""
	for l := range strings.SplitSeq(diff, "\n") {
		if after, ok := strings.CutPrefix(l, "+++ b/"); ok {
			current = after
			continue
		}
		if current != file || !strings.HasPrefix(l, "@@ ") {
			continue
		}
		parts := strings.Split(l, " ")
		if len(parts) < 3 || !strings.HasPrefix(parts[2], "+") {
			continue
		}
		spec := strings.TrimPrefix(parts[2], "+")
		var start int
		count := 1
		if before, after, found := strings.Cut(spec, ","); found {
			start, _ = strconv.Atoi(before)
			count, _ = strconv.Atoi(after)
		} else {
			start, _ = strconv.Atoi(spec)
		}
		for i := range count {
			lines[start+i] = true
		}
	}
	return lines
}

// Diff must report every changed line in one coordinate system: the working
// tree's, which is what its consumers parse their files in.
//
// Diff used to concatenate `git diff origin/main...HEAD` with `git diff HEAD`.
// The first numbers its hunks against HEAD, the second against the working
// tree. An uncommitted edit that changes a file's line count shifts everything
// below it, so the committed hunks then point at whatever now occupies those
// numbers.
//
// Both directions of that are wrong, and this pins both. The false negative is
// the dangerous one: a function changed by a commit escapes the gate entirely,
// which reports success. Diff's own doc comment used to claim the union was a
// safe superset that only over-includes — this fixture is the counterexample.
func TestDiff_CommittedHunksUseWorkingTreeLineNumbers(t *testing.T) {
	dir := newRepo(t)

	// Seven lines; funcC is on line 7.
	const base = "package p\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n"
	writeFile(t, dir, "committed.go", base)
	gitIn(t, dir, "add", "committed.go")
	gitIn(t, dir, "commit", "-q", "-m", "base")

	// Pin origin/main at this commit: the bug lives in the three-dot
	// origin/main...HEAD form. Without the ref, baseRange falls back to the
	// two-dot HEAD~1, which is already in working-tree coordinates and would
	// hide the defect.
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	// Commit a change to funcC, still on line 7.
	writeFile(t, dir, "committed.go", strings.Replace(base, "func C() {}", "func C() { _ = 1 }", 1))
	gitIn(t, dir, "add", "committed.go")
	gitIn(t, dir, "commit", "-q", "-m", "change C")

	// Now an *uncommitted* edit that prepends 5 lines, pushing funcC to 12.
	committed, err := os.ReadFile(filepath.Join(dir, "committed.go"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	writeFile(t, dir, "committed.go", "// 1\n// 2\n// 3\n// 4\n// 5\n"+string(committed))

	diff, err := Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := changedLines(t, diff, "committed.go")

	// funcC is committed-changed and now lives on line 12.
	if !got[12] {
		t.Errorf("Diff() does not mark line 12, where the committed change to func C now lives; a gate parsing the working tree would skip it entirely.\nreported lines: %v\ndiff:\n%s", got, diff)
	}
	// Line 7 is now a blank line nothing touched. Reporting it sends a gate
	// after code the change never affected.
	if got[7] {
		t.Errorf("Diff() marks line 7, which the committed hunk's stale HEAD-relative numbering points at; nothing changed there.\nreported lines: %v\ndiff:\n%s", got, diff)
	}
}

// The CI path: a clean working tree, where the only thing in scope is the
// committed range. Switching from a two-diff union to a single diff against
// the merge base must not change what CI sees, since with nothing uncommitted
// the two formulations describe the same set — this pins that they still do.
func TestDiff_CleanTreeStillReportsCommittedHunks(t *testing.T) {
	dir := newRepo(t)

	const base = "package p\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n"
	writeFile(t, dir, "committed.go", base)
	gitIn(t, dir, "add", "committed.go")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	writeFile(t, dir, "committed.go", strings.Replace(base, "func C() {}", "func C() { _ = 1 }", 1))
	gitIn(t, dir, "add", "committed.go")
	gitIn(t, dir, "commit", "-q", "-m", "change C")

	diff, err := Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := changedLines(t, diff, "committed.go")

	// Nothing uncommitted, so no shift: funcC is on line 7 in both HEAD and
	// the working tree.
	if !got[7] {
		t.Errorf("Diff() does not mark line 7 on a clean tree; the committed change to func C would escape CI.\nreported lines: %v\ndiff:\n%s", got, diff)
	}
	if len(got) != 1 {
		t.Errorf("Diff() marks %v on a clean tree, want only line 7 — anything else means the merge-base diff is picking up unrelated lines.\ndiff:\n%s", got, diff)
	}
}

// Consolidating Diff onto a single `git diff` made that one call
// load-bearing: if it fails and the error is swallowed, Diff returns a diff
// with no hunks, which every consumer reads as "nothing changed" and reports
// as a pass. That is the silent-pass failure this package exists to remove,
// so a diff that was supposed to run and did not must be loud.
//
// diff.external pointing at a missing program fails `git diff` while leaving
// merge-base and rev-parse working, which is exactly the shape being guarded:
// a base was resolved, so the diff was expected to succeed.
func TestDiff_FailedBaseDiffIsAnErrorNotAnEmptyResult(t *testing.T) {
	dir := newRepo(t)
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	writeFile(t, dir, "committed.go", "package p\n\nfunc A() { _ = 1 }\n")
	gitIn(t, dir, "config", "diff.external", "/nonexistent/prog")

	_, err := Diff()
	if err == nil {
		t.Fatal("Diff() returned nil error when the base diff command failed; an empty diff reads as \"nothing changed\" and the gate passes")
	}
}

// The other side of that: when there is genuinely no base to diff against —
// a repo whose HEAD is unborn — there is nothing to fail at, and Diff must
// still report untracked work rather than erroring the gate out.
func TestDiff_UnbornHeadDegradesQuietly(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main", dir)
	t.Chdir(dir)
	writeFile(t, dir, "brand_new.go", "package p\n\nfunc B() {}\n")

	diff, err := Diff()
	if err != nil {
		t.Fatalf("Diff() errored on an unborn HEAD, where having no base is normal: %v", err)
	}
	if !strings.Contains(diff, "+++ b/brand_new.go") {
		t.Errorf("Diff() dropped the untracked file on an unborn HEAD:\n%s", diff)
	}
}

// The middle rung of baseCommit's fallback chain: a branch with history but
// no origin/main, as in a shallow clone or an unfetched remote. Covered
// separately because the other two rungs take different code paths.
func TestDiff_FallsBackToHeadTildeOneWithoutOriginMain(t *testing.T) {
	dir := newRepo(t)

	const base = "package p\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n"
	writeFile(t, dir, "committed.go", base)
	gitIn(t, dir, "add", "committed.go")
	gitIn(t, dir, "commit", "-q", "-m", "base")

	// No origin/main ref is created, so merge-base fails and HEAD~1 is used.
	writeFile(t, dir, "committed.go", strings.Replace(base, "func C() {}", "func C() { _ = 1 }", 1))
	gitIn(t, dir, "add", "committed.go")
	gitIn(t, dir, "commit", "-q", "-m", "change C")

	diff, err := Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := changedLines(t, diff, "committed.go"); !got[7] {
		t.Errorf("Diff() does not mark line 7 via the HEAD~1 fallback; the committed change would escape the gate.\nreported lines: %v\ndiff:\n%s", got, diff)
	}
}

// git quotes non-ASCII paths C-style by default (core.quotePath), which
// breaks this package in two places at once. `ls-files --others` emits
// "caf\303\251.go" instead of café.go, which is not a path the --no-index
// diff can open — so the file is dropped and the error tolerated. And a diff
// header comes out as `+++ "b/caf\303\251.go"`, which does not match the
// `+++ b/` prefix consumers split on — so the file's line ranges go
// unattributed even when the diff did run.
//
// Both are silent. The tracked case matters more than the untracked one: it
// is the ordinary path for a gate, and there the file is present in the diff
// yet invisible to the parser reading it.
func TestNonASCIIPathsAreUsableNotQuoted(t *testing.T) {
	t.Run("untracked", func(t *testing.T) {
		dir := newRepo(t)
		writeFile(t, dir, "café.go", "package p\n\nfunc B() {}\n")

		files, err := Files()
		if err != nil {
			t.Fatalf("Files: %v", err)
		}
		if !slices.Contains(files, "café.go") {
			t.Errorf("Files() = %v, want the untracked file as a usable path, not git's C-quoted form", files)
		}

		diff, err := Diff()
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if !strings.Contains(diff, "+++ b/café.go") {
			t.Errorf("Diff() did not emit a parseable header for the non-ASCII untracked file:\n%s", diff)
		}
	})

	t.Run("tracked and committed", func(t *testing.T) {
		dir := newRepo(t)
		writeFile(t, dir, "café.go", "package p\n\nfunc B() {}\n")
		gitIn(t, dir, "add", ".")
		gitIn(t, dir, "commit", "-q", "-m", "add café")
		gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

		writeFile(t, dir, "café.go", "package p\n\nfunc B() { _ = 1 }\n")

		diff, err := Diff()
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if !strings.Contains(diff, "+++ b/café.go") {
			t.Errorf("Diff() emitted a C-quoted header for the tracked non-ASCII file, which no `+++ b/` parser will match:\n%s", diff)
		}
		if got := changedLines(t, diff, "café.go"); !got[3] {
			t.Errorf("changed line 3 of the non-ASCII file is unattributed; a gate would skip it.\nreported lines: %v\ndiff:\n%s", got, diff)
		}
	})
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
