# Red check for -affected selection (scripts/mutate/affected.go): each mutation
# neuters one decision and a named test must die.
#
#     go run ./scripts/mutate scripts/mutate/testdata/affected.spec
pkg ./scripts/mutate/
run TestChangedSince_|TestAffectedSpecs_
timeout 5m

[untracked files are not counted as changed]
file scripts/mutate/affected.go
--- anchor
	untracked, err := git("ls-files", "--others", "--exclude-standard", "-z")
--- replace
	untracked, err := "", error(nil)
--- end

[only the last commit is compared, not everything since ref]
file scripts/mutate/affected.go
--- anchor
"--end-of-options", ref, "--")
--- replace
"--end-of-options", "HEAD", "--")
--- end

[a changed spec file does not select itself]
file scripts/mutate/affected.go
--- anchor
	if _, ok := changedSet[path.Clean(s)]; ok {
--- replace
	if false {
--- end

[a changed mutated file does not select its spec]
file scripts/mutate/affected.go
--- anchor
		if _, ok := changedSet[path.Clean(m.file)]; ok {
--- replace
		if _, ok := changedSet[path.Clean(m.file)]; ok && false {
--- end

[a spec that cannot be read is skipped instead of reported]
file scripts/mutate/affected.go
--- anchor
		return nil, fmt.Errorf("%s: %w", s, err)
--- replace
		continue
--- end

[a ref is handed to git where it can be read as an option]
file scripts/mutate/affected.go
--- anchor
"-z", "--end-of-options", ref, "--")
--- replace
"-z", ref, "--")
--- end

[git's rename detection hides the old path of a renamed file]
file scripts/mutate/affected.go
--- anchor
"--name-only", "--no-renames", "-z",
--- replace
"--name-only", "-z",
--- end
