package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// testNameRe matches a bare Go test function name: what a spec's `run` line
// names when it is a plain alternation, and what `go test -list ^Test`
// prints for each declared test.
var testNameRe = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)

// plainAlternation splits a spec's `run` line into its alternatives when it
// is nothing but test names joined by `|`, in one of three shapes:
//
//   - a bare list, `A|B` — unanchored on both ends
//   - the whole thing wrapped, `^(A|B)$` — a degenerate case of the next
//     shape with an empty prefix
//   - a shared prefix outside the group, `<prefix>(A|B)$`, optionally with a
//     leading `^` — `prefix` is prepended to every alternative before it is
//     validated and returned, so `Test(A|B)$` yields `TestA`, `TestB`
//
// It reports which end(s) the trailing `$`/leading `^` anchor, for selects to
// rebuild the per-alternative pattern with. It reports ok=false for anything
// else — a pattern carrying other regexp syntax such as `.` or a subtest
// `/`, or an unbalanced/malformed use of `(`/`^`/`$` — and those fall back to
// the baseline's existing ranNothing check, which already refuses a `run`
// that matches nothing at all.
func plainAlternation(run string) (alts []string, startAnchored, endAnchored, ok bool) {
	// No explicit run == "" guard: splitting "" on "|" yields [""], and
	// testNameRe never matches the empty string, so the loop below already
	// returns false for it — a separate check here would be dead code that no
	// mutation could discriminate.
	s := run
	if strings.HasPrefix(s, "^") {
		startAnchored = true
		s = s[1:]
	}
	if idx := strings.IndexByte(s, '('); idx >= 0 && strings.HasSuffix(s, ")$") {
		prefix := s[:idx]
		endAnchored = true
		parts := strings.Split(s[idx+1:len(s)-2], "|")
		alts = make([]string, 0, len(parts))
		for _, p := range parts {
			name := prefix + p
			if !testNameRe.MatchString(name) {
				return nil, false, false, false
			}
			alts = append(alts, name)
		}
		return alts, startAnchored, endAnchored, true
	}
	if startAnchored {
		// A leading `^` with no `(`…`)$` group — e.g. the single-name
		// "^TestFoo$", or a bare alternation with a stray leading anchor —
		// is not one of the three shapes above, so it falls back rather than
		// being guessed at.
		return nil, false, false, false
	}
	parts := strings.Split(s, "|")
	for _, p := range parts {
		if !testNameRe.MatchString(p) {
			return nil, false, false, false
		}
	}
	return parts, false, false, true
}

// selects reports whether go test's -run would select at least one listed
// test for alt, by rebuilding the exact single-alternative pattern -run
// would have evaluated — alt with a leading `^` and/or trailing `$` added
// back per startAnchored/endAnchored — and asking the regexp package itself,
// rather than re-deriving containment/equality by hand. alt holds no regexp
// metacharacters (every return path through plainAlternation validated it
// against testNameRe), so this compiles unconditionally.
func selects(listed []string, alt string, startAnchored, endAnchored bool) bool {
	pat := alt
	if endAnchored {
		pat += "$"
	}
	if startAnchored {
		pat = "^" + pat
	}
	re := regexp.MustCompile(pat)
	return slices.ContainsFunc(listed, re.MatchString)
}

// listArgs builds the argv for listing a package's declared tests, honouring
// the spec's build tags the same way testArgs does for the baseline run.
func listArgs(sp *spec) []string {
	args := []string{"test", "-list", "^Test"}
	if sp.tags != "" {
		args = append(args, "-tags="+sp.tags)
	}
	args = append(args, sp.pkg)
	return args
}

// listTests names every top-level Test function `go test -list` reports for
// the spec's package. It excludes Example, Benchmark and Fuzz entries: a
// spec's `run` line in this format only ever names tests.
//
// This compiles the test binary — `go test -list` still builds the package
// to enumerate its tests — which is why it runs once, before the baseline,
// rather than for every mutation.
func listTests(root string, sp *spec) ([]string, error) {
	cmd := goCommand(context.Background(), root, listArgs(sp))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list tests in %s: %w\n%s", sp.pkg, err, indent(string(out)))
	}
	var names []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if s := strings.TrimSpace(line); testNameRe.MatchString(s) {
			names = append(names, s)
		}
	}
	return names, nil
}

// filterMatchesName reports whether go test's `-run` pattern would select a
// test named name — a bare top-level name or a `/`-separated subtest path —
// the way `go test` itself does: split both run and name on `/` and match
// each level's segment against the corresponding name segment, unanchored.
// name may have more segments than run (a subtest under a top-level name
// `run` selects by name alone; the unconstrained levels select too, which is
// why only min(len(runParts), len(nameParts)) levels are checked) or fewer
// (a bare top-level failure against a deeper `run` line still has its one
// segment checked).
//
// Unlike plainAlternation/selects above, this classifies one already-observed
// failing test name against the spec's `run` line as written; it does not
// require `run` to be a plain alternation of test names first.
func filterMatchesName(run, name string) bool {
	runParts := strings.Split(run, "/")
	nameParts := strings.Split(name, "/")
	n := min(len(runParts), len(nameParts))
	for i := range n {
		re, err := regexp.Compile(runParts[i])
		if err != nil {
			// A fragment go test's own flag parsing would have rejected cannot
			// have selected anything; report non-selecting rather than
			// panicking on a `run` line that was never a valid regexp to
			// begin with.
			return false
		}
		if !re.MatchString(nameParts[i]) {
			return false
		}
	}
	return true
}

// deadFilterAlternatives checks a list of test names against the spec's parsed
// run filter alternatives. It returns nil for a run that is not a plain
// alternation.
func deadFilterAlternatives(listed []string, sp *spec) []string {
	alts, startAnchored, endAnchored, ok := plainAlternation(sp.run)
	if !ok {
		return nil
	}
	var dead []string
	for _, alt := range alts {
		if !selects(listed, alt, startAnchored, endAnchored) {
			dead = append(dead, alt)
		}
	}
	return dead
}

// deadRunFilterNames names every alternative of a plain `run` line that
// selects no test `go test -list` reports for the spec's package. It returns
// nil, nil for a `run` that is not a plain alternation (see
// plainAlternation) or that names none at all.
func deadRunFilterNames(root string, sp *spec) ([]string, error) {
	if _, _, _, ok := plainAlternation(sp.run); !ok {
		return nil, nil
	}
	listed, err := listTests(root, sp)
	if err != nil {
		return nil, fmt.Errorf("list tests for the `run` check: %w", err)
	}
	return deadFilterAlternatives(listed, sp), nil
}

// reportRunFilter prints the RUNFILTER verdict for every dead alternative and
// returns the process exit code. It is not a mutation report — nothing was
// applied and nothing needs restoring — so it does not go through report(),
// whose "N of M mutations" summary would misdescribe a `run` line as though
// it were a mutation.
func reportRunFilter(pkg string, dead []string) int {
	fmt.Printf("%-13s  %s\n", "verdict", "evidence")
	fmt.Println(strings.Repeat("-", 13+2+60))
	for _, name := range dead {
		fmt.Printf("%-13s  %s does not name a test in %s\n", runFilter, name, pkg)
	}
	fmt.Printf("\nStatus: %d name(s) in `run` select no test.\n"+
		"go test drops a dead alternative of a `|`-separated filter silently\n"+
		"rather than erroring, so the remaining alternatives still select real\n"+
		"tests and the baseline still passes. Fix the spelling or remove the\n"+
		"name(s).\n", len(dead))
	return 1
}
