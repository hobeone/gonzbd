# The red check for this command, run by the command itself.
#
# Each mutation neuters one invariant the tool exists to enforce, and the test
# named beside it must die. Run it after changing anything in main.go:
#
#     go run ./scripts/mutate scripts/mutate/testdata/self.spec
#
# Every mutation neuters a condition rather than deleting a block, per
# AGENTS.md — a deletion usually breaks the build, and COMPILE_ERROR is not
# evidence that a test discriminates.
pkg ./scripts/mutate/
timeout 5m

[-count=1 dropped from the test command]
file scripts/mutate/main.go
--- anchor
	args := []string{"test", "-count=1", sp.pkg}
--- replace
	args := []string{"test", sp.pkg}
--- end

[build-failure detection widened to a substring scan]
file scripts/mutate/main.go
--- anchor
var buildFailedRe = regexp.MustCompile(`(?m)^FAIL\s+\S+\s+\[(?:build|setup) failed\]`)
--- replace
var buildFailedRe = regexp.MustCompile(`\[(?:build|setup) failed\]`)
--- end

[compiler diagnostics matched without a column]
file scripts/mutate/main.go
--- anchor
var compileErrRe = regexp.MustCompile(`^\S*\.go:\d+:\d+: .+`)
--- replace
var compileErrRe = regexp.MustCompile(`^\S*\.go:\d+: .+`)
--- end

[restore trusts the write instead of proving it]
file scripts/mutate/main.go
--- anchor
	if !bytes.Equal(got, original) {
		return fmt.Errorf("file differs from the original after restore")
	}
--- replace
	if false && !bytes.Equal(got, original) {
		return fmt.Errorf("file differs from the original after restore")
	}
--- end

[a no-op mutation is accepted]
file scripts/mutate/spec.go
--- anchor
		case cur.anchor == cur.replace:
--- replace
		case false && cur.anchor == cur.replace:
--- end

[anchor uniqueness not required]
file scripts/mutate/main.go
--- anchor
	switch n := strings.Count(content, anchor); n {
	case 1:
		return nil
--- replace
	switch n := strings.Count(content, anchor); n {
	case 1, 2:
		return nil
--- end

[containment dropped: paths may escape the repository]
file scripts/mutate/main.go
--- anchor
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
--- replace
	if rel == ".." && false {
--- end

[an empty anchor is accepted]
file scripts/mutate/spec.go
--- anchor
		case cur.anchor == "":
--- replace
		case false && cur.anchor == "":
--- end

[a run filter matching no test passes the baseline]
file scripts/mutate/main.go
--- anchor
	return ranNothingRe.MatchString(out) || strings.Contains(out, "warning: no tests to run")
--- replace
	return ranNothingRe.MatchString(out) && strings.Contains(out, "warning: no tests to run")
--- end

[a launch failure is reported as a test failure]
file scripts/mutate/main.go
--- anchor
		ee, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			return string(out), 0, err
		}
--- replace
		ee, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			return string(out), 1, nil
		}
--- end

[setup logs may donate the evidence line]
file scripts/mutate/main.go
--- anchor
	for _, line := range lines[start:] {
--- replace
	for _, line := range lines[start*0:] {
--- end

[a symlink may escape the repository]
file scripts/mutate/main.go
--- anchor
	realAbs, err := filepath.EvalSymlinks(abs)
--- replace
	realAbs, err := abs, error(nil)
--- end

[a CRLF spec keeps its carriage returns]
file scripts/mutate/spec.go
--- anchor
		raw := strings.TrimRight(rawLine, "\r")
--- replace
		raw := rawLine
--- end

[a missing replace section is accepted]
file scripts/mutate/spec.go
--- anchor
		case !sawReplace:
--- replace
		case false && !sawReplace:
--- end

[a global directive inside a block is accepted]
file scripts/mutate/spec.go
--- anchor
		if cur != nil && key != "file" {
--- replace
		if false && cur != nil && key != "file" {
--- end

[a passing mutation is never widened past the run filter]
file scripts/mutate/main.go
--- anchor
	if sp.run == "" {
--- replace
	if true {
--- end

[the wider run is made even when the spec has no filter]
file scripts/mutate/main.go
--- anchor
	if sp.run == "" {
--- replace
	if false {
--- end

[a confirming run that never starts is treated as a red package]
file scripts/mutate/main.go
--- anchor
	if launchErr != nil {
		return nil, fmt.Errorf("could not run the confirming package-wide test: %w", launchErr)
	}
--- replace
	if launchErr != nil && false {
		return nil, fmt.Errorf("could not run the confirming package-wide test: %w", launchErr)
	}
--- end

[an exit between the write and the verdict leaves the tree mutated]
file scripts/mutate/main.go
--- anchor
	if rerr := restore(path, backup, original); rerr != nil {
		fatal("%s\nRESTORE ALSO FAILED: %v\nRecover from %s.", msg, rerr, backup)
	}
--- replace
	if rerr := error(nil); rerr != nil {
		fatal("%s\nRESTORE ALSO FAILED: %v\nRecover from %s.", msg, rerr, backup)
	}
--- end

[an exclusion is claimed even when the package is green]
file scripts/mutate/main.go
--- anchor
	if launchErr != nil || code == 0 || buildFailed(out) {
--- replace
	if launchErr != nil || (code == 0 && false) || buildFailed(out) {
--- end

[the confirmation runs whether or not anything claimed an exclusion]
file scripts/mutate/main.go
--- anchor
	return slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded || r.verdict == flaky })
--- replace
	return true || slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded || r.verdict == flaky })
--- end

[an exclusion is trusted without confirming the package is green unmutated]
file scripts/mutate/main.go
--- anchor
	if code == 0 && !ranNothing(out) {
--- replace
	if true || (code == 0 && !ranNothing(out)) {
--- end

[a failing subtest is named instead of its parent]
file scripts/mutate/main.go
--- anchor
		name, _, _ := strings.Cut(p, "/")
--- replace
		name := p
--- end

[the backup temp dir leaks on a failed write]
file scripts/mutate/main.go
--- anchor
		_ = os.RemoveAll(dir)
		return "", err
--- replace
		return "", err
--- end

[filterMatchesName never shortens n to the name's segment count]
file scripts/mutate/runfilter.go
--- anchor
	n := min(len(runParts), len(nameParts))
--- replace
	n := len(runParts)
--- end

[filterMatchesName matches through an invalid regexp fragment instead of refusing it]
file scripts/mutate/runfilter.go
--- anchor
		re, err := regexp.Compile(runParts[i])
		if err != nil {
--- replace
		re, err := regexp.Compile(runParts[i])
		if false && err != nil {
--- end

[a FLAKY verdict loses its own note and reads as an unexplained blank]
file scripts/mutate/main.go
--- anchor
	case flaky:
		return "the test named in the evidence column is already selected by `run` —\n" +
			"  widening the filter changes nothing. It killed this mutation once and\n" +
			"  passed on it once, so the inconsistency is in the test, not the spec.\n" +
			"  Investigate that test's determinism before trusting either run of it."
--- replace
	case flaky:
		return ""
--- end

[a leading caret is never stripped]
file scripts/mutate/runfilter.go
--- anchor
	if strings.HasPrefix(s, "^") {
--- replace
	if false {
--- end

[a leading caret is recorded as unanchored]
file scripts/mutate/runfilter.go
--- anchor
		startAnchored = true
--- replace
		startAnchored = false
--- end

[a prefixed group's trailing $ is not required]
file scripts/mutate/runfilter.go
--- anchor
	if idx := strings.IndexByte(s, '('); idx >= 0 && strings.HasSuffix(s, ")$") {
--- replace
	if idx := strings.IndexByte(s, '('); idx >= 0 {
--- end

[a prefixed group's alternatives forget their shared prefix]
file scripts/mutate/runfilter.go
--- anchor
			name := prefix + p
--- replace
			name := p + prefix[:0]
--- end

[an invalid expansion of a prefixed group is accepted]
file scripts/mutate/runfilter.go
--- anchor
			if !testNameRe.MatchString(name) {
--- replace
			if false {
--- end

[a prefixed group is never marked end-anchored]
file scripts/mutate/runfilter.go
--- anchor
		endAnchored = true
--- replace
		endAnchored = false
--- end

[a leading caret without a group is accepted instead of falling back]
file scripts/mutate/runfilter.go
--- anchor
	if startAnchored {
		// A leading `^` with no `(`…`)$` group — e.g. the single-name
--- replace
	if false {
		// A leading `^` with no `(`…`)$` group — e.g. the single-name
--- end

[a test name pattern matches a subtest path too]
file scripts/mutate/runfilter.go
--- anchor
var testNameRe = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)
--- replace
var testNameRe = regexp.MustCompile(`^Test[A-Za-z0-9_]*`)
--- end

[selects ignores the end anchor]
file scripts/mutate/runfilter.go
--- anchor
	if endAnchored {
		pat += "$"
	}
--- replace
	if false && endAnchored {
		pat += "$"
	}
--- end

[selects ignores the start anchor]
file scripts/mutate/runfilter.go
--- anchor
	if startAnchored {
		pat = "^" + pat
	}
--- replace
	if false && startAnchored {
		pat = "^" + pat
	}
--- end

[selects never matches anything]
file scripts/mutate/runfilter.go
--- anchor
	re := regexp.MustCompile(pat)
	return slices.ContainsFunc(listed, re.MatchString)
--- replace
	re := regexp.MustCompile(pat)
	return slices.ContainsFunc(listed, re.MatchString) && false
--- end

[a run filter alternative that matches a real test is reported dead, and vice versa]
file scripts/mutate/runfilter.go
--- anchor
		if !selects(listed, alt, startAnchored, endAnchored) {
--- replace
		if selects(listed, alt, startAnchored, endAnchored) {
--- end

[-check accepts an anchor that does not resolve to exactly one site]
file scripts/mutate/check.go
--- anchor
		if n := strings.Count(content, m.anchor); n != 1 {
--- replace
		if n := strings.Count(content, m.anchor); false {
--- end

[-check reports issues but still exits 0]
file scripts/mutate/check.go
--- anchor
	if len(issues) > 0 {
		fmt.Printf("\nStatus: %d anchor(s) across %d spec(s) do not resolve to exactly one site.\n", len(issues), len(actual))
		ok = false
	}
--- replace
	if len(issues) > 0 {
		fmt.Printf("\nStatus: %d anchor(s) across %d spec(s) do not resolve to exactly one site.\n", len(issues), len(actual))
		ok = true
	}
--- end

[-check-all discovers nothing: the git ls-files pathspec is wrong]
file scripts/mutate/check.go
--- anchor
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "--", "*testdata/*.spec")
--- replace
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "--", "*nonexistent-pattern*.spec")
--- end

[a banner-less package failure is read as FLAKY on an empty test name]
file scripts/mutate/main.go
--- anchor
	if len(names) == 0 {
--- replace
	if false && len(names) == 0 {
--- end

[a subtest's failure is credited to the whole top-level name when excluding it]
file scripts/mutate/main.go
--- anchor
			top, _, _ := strings.Cut(p, "/")
--- replace
			top := p
--- end

[a real spec gap is never reported once any failure is FLAKY-shaped]
file scripts/mutate/main.go
--- anchor
	if len(left) > 0 {
--- replace
	if true {
--- end

[an excluded test's gap is hidden behind a FLAKY verdict]
file scripts/mutate/main.go
--- anchor
	if len(left) > 0 {
--- replace
	if false {
--- end

[every failing test is treated as already selected by `run`]
file scripts/mutate/main.go
--- anchor
		if !filterMatchesName(run, p) {
--- replace
		if false {
--- end

[every failing test is treated as excluded, so FLAKY is never reached]
file scripts/mutate/main.go
--- anchor
		if !filterMatchesName(run, p) {
--- replace
		if true {
--- end

[a FLAKY row is never confirmed against the unmutated package]
file scripts/mutate/main.go
--- anchor
		if results[i].verdict == excluded || results[i].verdict == flaky {
--- replace
		if results[i].verdict == excluded {
--- end

[an EXCLUDED row is never confirmed against the unmutated package]
file scripts/mutate/main.go
--- anchor
		if results[i].verdict == excluded || results[i].verdict == flaky {
--- replace
		if results[i].verdict == flaky {
--- end

[a spec with only a FLAKY row skips the confirming run]
file scripts/mutate/main.go
--- anchor
	return slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded || r.verdict == flaky })
--- replace
	return slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded })
--- end

[go subprocesses stop being pointed at the throwaway cache]
file scripts/mutate/gocache.go
--- anchor
	if dir := cacheDir(); dir != "" {
		// A duplicate key
--- replace
	if dir := cacheDir(); false && dir != "" {
		// A duplicate key
--- end

[exit skips removing the throwaway cache]
file scripts/mutate/main.go
--- anchor
	cleanupThrowaway()
	os.Exit(code)
--- replace
	os.Exit(code)
--- end

[a panic in runSpec no longer removes the throwaway cache]
file scripts/mutate/main.go
--- anchor
	defer cleanupThrowaway() // a panic unwinds through here; every os.Exit goes through exit
--- replace
	defer func() {}() // a panic unwinds through here; every os.Exit goes through exit
--- end

[action entries hardlinked instead of copied when seeding]
file scripts/mutate/gocache.go
--- anchor
			if err := os.WriteFile(to, b, 0o666); err != nil { //nolint:gosec // G306: matches the mode go gives action entries
				return err
			}
--- replace
			_ = b
			if err := os.Link(from, to); err != nil {
				return err
			}
--- end

[removal guard on the cache directory's parent neutered]
file scripts/mutate/gocache.go
--- anchor
	if filepath.Dir(filepath.Clean(dir)) != base || !strings.HasPrefix(filepath.Base(dir), cacheDirPrefix) {
--- replace
	if false {
--- end

[the cache directory is never registered for removal]
file scripts/mutate/gocache.go
--- anchor
	throwaway.dir, throwaway.base = dir, filepath.Clean(base)
--- replace
	throwaway.dir, throwaway.base = "", ""
--- end
