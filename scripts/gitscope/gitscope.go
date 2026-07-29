// Package gitscope resolves which files a dev gate should examine.
//
// The repo's three gates (check_coverage, check_test_alignment,
// check_lock_io) are diff-scoped: they inspect what changed rather than the
// whole tree. Each used to derive that set on its own, from the committed
// range alone, which meant a gate run before committing silently reported
// "No source Go files found to check" — a green result that had checked
// nothing. That failure mode is indistinguishable from a genuine pass, and it
// bites hardest exactly when a gate is most useful: mid-edit, before the
// commit.
//
// The scope here is the union of:
//
//   - committed changes against the base branch (origin/main, or HEAD~1 when
//     that ref is unavailable, e.g. a shallow clone),
//   - staged and unstaged changes in the working tree,
//   - untracked files.
//
// In CI the working tree is clean, so the union collapses to the committed
// range and behaviour is unchanged there.
package gitscope

import (
	"bytes"
	"os/exec"
	"sort"
	"strings"
)

// baseRange returns the revision range for committed changes. It prefers
// origin/main and falls back to HEAD~1 when that ref does not resolve.
//
// The fallback is also correct for the very first commit on a branch, where
// HEAD~1 exists but origin/main may not have been fetched.
func baseRange() string {
	if err := exec.Command("git", "rev-parse", "--verify", "--quiet", "origin/main").Run(); err == nil {
		return "origin/main...HEAD"
	}
	return "HEAD~1"
}

// git runs a git command and returns stdout. A non-zero exit is reported
// alongside whatever output was produced, so callers can choose to tolerate
// it — several of these commands fail benignly (no HEAD~1 in a fresh repo,
// or `diff --no-index` exiting 1 simply because files differ).
func git(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", args...) //nolint:gosec // dev tool, callers pass fixed args
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// Untracked returns repo-relative paths of files git does not track,
// respecting .gitignore.
func Untracked() ([]string, error) {
	out, err := git("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// Files returns the repo-relative paths in scope, deduplicated and sorted.
//
// Errors from the individual git invocations are deliberately tolerated: a
// missing HEAD~1 or an unfetched origin/main should degrade the scope, not
// fail the gate outright. A caller that ends up with an empty set should say
// so plainly rather than reporting a pass.
func Files() ([]string, error) {
	seen := make(map[string]struct{})

	// Committed range, then everything not yet committed.
	if out, err := git("diff", "--name-only", baseRange()); err == nil {
		addAll(seen, splitLines(out))
	}
	if out, err := git("diff", "--name-only", "HEAD"); err == nil {
		addAll(seen, splitLines(out))
	}
	untracked, err := Untracked()
	if err != nil {
		return nil, err
	}
	addAll(seen, untracked)

	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// Diff returns a -U0 unified diff covering the same scope as Files, for
// callers that need changed line ranges rather than paths.
//
// Untracked files are rendered via `git diff --no-index` against /dev/null,
// which emits an ordinary "+++ b/<path>" header and a single hunk covering
// the whole file. That keeps them visible to a plain unified-diff parser
// without the caller special-casing them.
//
// Because the committed range and the working-tree diff are numbered against
// different snapshots, an uncommitted edit that shifts lines can leave both
// sets of numbers in the output. The result is a superset of the truly
// changed lines, which is the safe direction for a gate: it over-includes
// rather than letting a changed line escape scrutiny.
func Diff() (string, error) {
	var b strings.Builder

	if out, err := git("diff", "-U0", baseRange()); err == nil {
		b.WriteString(out)
	}
	if out, err := git("diff", "-U0", "HEAD"); err == nil {
		b.WriteString(out)
	}

	untracked, err := Untracked()
	if err != nil {
		return "", err
	}
	for _, f := range untracked {
		// --no-index exits 1 whenever the inputs differ, which is always true
		// here, so the error is expected and the output is what matters.
		out, _ := git("diff", "--no-index", "-U0", "/dev/null", f)
		b.WriteString(out)
	}
	return b.String(), nil
}

func splitLines(s string) []string {
	var lines []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func addAll(set map[string]struct{}, items []string) {
	for _, it := range items {
		set[it] = struct{}{}
	}
}
