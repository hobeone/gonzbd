package main

import (
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// changedSince lists the repository-relative paths whose content differs from
// ref in this checkout: tracked files compared with the working tree (so
// committed, staged and unstaged edits all count) plus untracked files that
// are not gitignored. A path deleted since ref is listed, and so are both
// sides of a rename (--no-renames), since a spec may mutate the old path.
//
// It does not use scripts/gitscope, whose Files compares against a fixed base
// and takes no ref, and which has no way to run in another directory.
func changedSince(root, ref string) ([]string, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...) //nolint:gosec // G204: argv is fixed apart from ref, which follows --end-of-options and comes from the operator's own command line
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return string(out), nil
	}
	tracked, err := git("diff", "--name-only", "--no-renames", "-z", "--end-of-options", ref, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var changed []string
	for p := range strings.SplitSeq(tracked+untracked, "\x00") {
		if p != "" {
			changed = append(changed, path.Clean(p))
		}
	}
	slices.Sort(changed)
	return slices.Compact(changed), nil
}

// affectedSpecs keeps the specs whose verdicts a change to changed can move
// by this rule: the spec file itself changed, or a file one of its mutations
// edits changed. A change to any other file selects nothing, including the
// test the spec runs and the package it targets, so the result is a fast loop
// and not a gate: it can miss a spec whose pinned behaviour such a file
// altered. specs are repository-relative paths as discoverSpecs returns them.
func affectedSpecs(root string, specs, changed []string) ([]string, error) {
	changedSet := make(map[string]struct{}, len(changed))
	for _, c := range changed {
		changedSet[c] = struct{}{}
	}
	var out []string
	for _, s := range specs {
		if _, ok := changedSet[path.Clean(s)]; ok {
			out = append(out, s)
			continue
		}
		sp, err := parseSpec(filepath.Join(root, s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
		for _, m := range sp.mutations {
			if _, ok := changedSet[path.Clean(m.file)]; ok {
				out = append(out, s)
				break
			}
		}
	}
	return out, nil
}
