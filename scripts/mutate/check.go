package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// anchorIssue is one anchor that did not resolve to exactly one site.
type anchorIssue struct {
	spec     string // the spec path as given to -check, or discovered by -check-all
	mutation string
	file     string
	count    int
}

// checkSpecAnchors parses one spec and resolves every anchor against the
// current source, without writing anything or running any test. It is
// checkAnchor's own count — strings.Count(content, anchor) — read for every
// mutation a spec declares, collected up front instead of discovered one
// ANCHOR verdict at a time as `run` would apply the mutations in sequence.
//
// specPath is what parseSpec reads; displaySpec is what issues are reported
// against. They differ under -check-all, where specPath is joined against
// root so the spec can be read regardless of the process's own working
// directory, but the message should still name the path the way git ls-files
// printed it.
func checkSpecAnchors(root, displaySpec, specPath string) ([]anchorIssue, error) {
	sp, err := parseSpec(specPath)
	if err != nil {
		return nil, err
	}

	// Reading each file once, rather than once per mutation, matters for a
	// spec like stamp_owner.spec whose eight mutations land in one file.
	contents := map[string]string{}
	var issues []anchorIssue
	for _, m := range sp.mutations {
		content, ok := contents[m.file]
		if !ok {
			path, rerr := resolve(root, m.file)
			if rerr != nil {
				return nil, fmt.Errorf("%s: [%s]: %w", displaySpec, m.name, rerr)
			}
			data, rerr := os.ReadFile(path) //nolint:gosec // G304: path is checked by resolve to be inside the repository
			if rerr != nil {
				return nil, fmt.Errorf("%s: [%s]: %w", displaySpec, m.name, rerr)
			}
			content = string(data)
			contents[m.file] = content
		}
		if n := strings.Count(content, m.anchor); n != 1 {
			issues = append(issues, anchorIssue{spec: displaySpec, mutation: m.name, file: m.file, count: n})
		}
	}
	return issues, nil
}

// discoverSpecs finds every mutation spec belonging to this checkout, the
// same way scripts/run_tests.sh does: `git ls-files`, not `find` or
// filepath.Walk. Both of those descend into the gitignored
// `.claude/worktrees/` — a sibling checkout, possibly on another branch —
// whose specs would then be read from there and checked against THIS tree's
// source, either producing a bogus "anchor matched no site" or, worse, a
// false-clean pass where the anchor text happens to exist in both branches.
// `git ls-files` cannot leave the current worktree.
//
// `--others --exclude-standard` keeps a newly written, not-yet-staged spec in
// scope, matching run_tests.sh's own reason for the same flags.
func discoverSpecs(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "--", "*testdata/*.spec")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("discover specs: %w", err)
	}
	var specs []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			specs = append(specs, s)
		}
	}
	slices.Sort(specs)
	return specs, nil
}

// runCheck implements -check and -check-all: it resolves every anchor in
// every named spec and reports the ones that do not resolve to exactly one
// site, without compiling anything, running a test, or writing to the
// working tree.
//
// display and actual are parallel slices of equal length. actual is what
// checkSpecAnchors reads; display is what an issue is reported against. They
// differ only under -check-all, where actual is joined against root so a
// spec can be read regardless of the process's working directory.
func runCheck(root string, display, actual []string) int {
	ok := true
	var issues []anchorIssue
	for i, specPath := range actual {
		found, err := checkSpecAnchors(root, display[i], specPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mutate: -check %s: %v\n", display[i], err)
			ok = false
			continue
		}
		issues = append(issues, found...)
	}

	for _, iss := range issues {
		if iss.count == 0 {
			fmt.Printf("%s [%s]: anchor matched no site in %s; it may be stale\n", iss.spec, iss.mutation, iss.file)
		} else {
			fmt.Printf("%s [%s]: anchor matched %d sites in %s, want exactly 1\n", iss.spec, iss.mutation, iss.count, iss.file)
		}
	}
	if len(issues) > 0 {
		fmt.Printf("\nStatus: %d anchor(s) across %d spec(s) do not resolve to exactly one site.\n", len(issues), len(actual))
		ok = false
	}
	if !ok {
		return 1
	}
	fmt.Printf("check: %d spec(s), every anchor resolves to exactly one site.\n", len(actual))
	return 0
}
