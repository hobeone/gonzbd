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
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// baseRange returns the revision range for committed changes. It prefers
// origin/main and falls back to HEAD~1 when that ref does not resolve.
//
// The fallback is also correct for the very first commit on a branch, where
// HEAD~1 exists but origin/main may not have been fetched.
//
// Used only by Files, which collects paths. Diff needs baseCommit instead —
// see its doc comment for why a range is the wrong shape there.
func baseRange() string {
	if err := exec.Command("git", "rev-parse", "--verify", "--quiet", "origin/main").Run(); err == nil {
		return "origin/main...HEAD"
	}
	return "HEAD~1"
}

// baseCommit returns a single commit to diff the working tree against, so
// that every hunk Diff emits is numbered against the working tree.
//
// This is deliberately a commit and not a range. `git diff A...B` compares
// the merge base to B, and when B is HEAD that means the committed snapshot
// rather than the files on disk. Diffing the working tree against the merge
// base directly answers the question the gates actually ask — "what does this
// branch change, as it stands right now" — in one pass and one coordinate
// system.
//
// The fallback chain degrades rather than failing: an unfetched origin/main
// falls back to HEAD~1, and a single-commit repo to HEAD, which still catches
// uncommitted work.
//
// The bool reports whether any base could be resolved at all. A repo whose
// HEAD is unborn has nothing committed to compare against, which is normal
// and not an error — but it must be distinguishable from a base that resolved
// and then failed to diff, because only the latter means a diff that should
// have run did not. See Diff.
func baseCommit() (string, bool) {
	if out, err := git("merge-base", "origin/main", "HEAD"); err == nil {
		if sha := strings.TrimSpace(out); sha != "" {
			return sha, true
		}
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "HEAD~1"); err == nil {
		return "HEAD~1", true
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		return "HEAD", true
	}
	return "", false
}

// git runs a git command and returns stdout. A non-zero exit is reported
// alongside whatever output was produced, so callers can choose to tolerate
// it — several of these commands fail benignly (no HEAD~1 in a fresh repo,
// or `diff --no-index` exiting 1 simply because files differ).
//
// core.quotePath is disabled for every invocation, because git otherwise
// renders any non-ASCII path in C-quoted form: café.go appears as
// "caf\303\251.go" in ls-files output and as `+++ "b/caf\303\251.go"` in a
// diff header. Neither is a path. The first cannot be opened, so Diff's
// --no-index call fails and drops the file; the second does not match the
// `+++ b/` prefix consumers split on, so the file's line ranges are silently
// unattributed. Setting it here rather than per-call covers both, and any
// path-bearing command added later.
func git(args ...string) (string, error) {
	var out bytes.Buffer
	full := append([]string{"-c", "core.quotePath=false"}, args...)
	cmd := exec.Command("git", full...) //nolint:gosec // dev tool, callers pass fixed args
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// Untracked returns repo-relative paths of files git does not track,
// respecting .gitignore. Paths come back usable rather than C-quoted; see
// the git helper.
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
// Every hunk is numbered against the working tree, because that is the file a
// consumer parses when it maps line numbers back to functions.
//
// This used to concatenate the committed range with the working-tree diff.
// Those are numbered against different snapshots, so an uncommitted edit that
// changed a file's line count shifted every committed hunk below it onto
// whatever now occupied those numbers. That was documented here as a safe
// superset that could only over-include; it was not. A function changed by a
// commit and then displaced by an uncommitted edit above it fell out of the
// set entirely, and the gate reported success — see
// TestDiff_CommittedHunksUseWorkingTreeLineNumbers, which pins both the
// false positive and the false negative.
//
// Unlike Files, a failure of the base diff is returned rather than tolerated.
// Diff has exactly one call covering all committed and uncommitted work, so
// swallowing its error yields a diff with no hunks — which every consumer
// reads as "nothing changed" and reports as a pass. An unresolvable base is
// the one benign case and is handled by the ok check, not by ignoring errors.
func Diff() (string, error) {
	var b strings.Builder

	if base, ok := baseCommit(); ok {
		out, err := git("diff", "-U0", base)
		if err != nil {
			return "", fmt.Errorf("gitscope: git diff -U0 %s: %w", base, err)
		}
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
