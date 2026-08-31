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
