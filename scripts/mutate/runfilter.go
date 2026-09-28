package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// testNameRe matches a bare Go test function name: what a spec's `run` line
// names when it is a plain alternation, and what `go test -list ^Test`
// prints for each declared test.
var testNameRe = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)

// plainAlternation splits a spec's `run` line into its alternatives when it
// is nothing but test names joined by `|`, optionally wrapped in `^(`…`)$`.
// It reports ok=false for anything else — a single unanchored name, a
// pattern carrying other regexp syntax such as `.` or a subtest `/` — and
// those fall back to the baseline's existing ranNothing check, which already
// refuses a `run` that matches nothing at all.
func plainAlternation(run string) (alts []string, ok bool) {
	// No explicit run == "" guard: splitting "" on "|" yields [""], and
	// testNameRe never matches the empty string, so the loop below already
	// returns false for it — a separate check here would be dead code that no
	// mutation could discriminate.
	s := run
	if strings.HasPrefix(s, "^(") && strings.HasSuffix(s, ")$") {
		s = s[2 : len(s)-2]
	}
	parts := strings.Split(s, "|")
	for _, p := range parts {
		if !testNameRe.MatchString(p) {
			return nil, false
		}
	}
	return parts, true
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
	cmd := exec.Command("go", listArgs(sp)...) //nolint:gosec // G204: argv comes from the operator's own spec, the same trust level as testArgs
	cmd.Dir = root
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

// deadRunFilterNames names every alternative of a plain `run` line that
// matches no test `go test -list` reports for the spec's package. It returns
// nil, nil for a `run` that is not a plain alternation (see
// plainAlternation) or that names none at all.
func deadRunFilterNames(root string, sp *spec) ([]string, error) {
	alts, ok := plainAlternation(sp.run)
	if !ok {
		return nil, nil
	}
	listed, err := listTests(root, sp)
	if err != nil {
		return nil, fmt.Errorf("list tests for the `run` check: %w", err)
	}
	var dead []string
	for _, alt := range alts {
		if !slices.Contains(listed, alt) {
			dead = append(dead, alt)
		}
	}
	return dead, nil
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
	fmt.Printf("\nStatus: %d name(s) in `run` match no test.\n"+
		"go test drops a dead alternative of a `|`-separated filter silently\n"+
		"rather than erroring, so the remaining alternatives still select real\n"+
		"tests and the baseline still passes. Fix the spelling or remove the\n"+
		"name(s).\n", len(dead))
	return 1
}
