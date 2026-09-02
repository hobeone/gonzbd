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
	return slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded })
--- replace
	return true || slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded })
--- end

[an exclusion is trusted without confirming the package is green unmutated]
file scripts/mutate/main.go
--- anchor
	if launchErr == nil && code == 0 && !ranNothing(out) {
--- replace
	if true || (launchErr == nil && code == 0 && !ranNothing(out)) {
--- end

[a failing subtest is named instead of its parent]
file scripts/mutate/main.go
--- anchor
		name, _, _ := strings.Cut(m[1], "/")
--- replace
		name := m[1]
--- end

[the backup temp dir leaks on a failed write]
file scripts/mutate/main.go
--- anchor
		_ = os.RemoveAll(dir)
		return "", err
--- replace
		return "", err
--- end
