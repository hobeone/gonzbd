# Red check for -affected selection (scripts/mutate/affected.go): each mutation
# neuters one decision and a named test must die.
#
#     go run ./scripts/mutate scripts/mutate/testdata/affected.spec
pkg ./scripts/mutate/
run TestChangedSince_|TestAffectedSpecs_|TestDepCache_|TestCallsGoOrGit
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

[a changed spec file does not select itself]
file scripts/mutate/affected.go
--- anchor
		if changedSpec[path.Clean(s)] {
--- replace
		if false && changedSpec[path.Clean(s)] {
--- end

[a spec that cannot be read is skipped instead of reported]
file scripts/mutate/affected.go
--- anchor
		sp, err := parseSpec(filepath.Join(root, s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
--- replace
		sp, err := parseSpec(filepath.Join(root, s))
		if err != nil {
			continue
		}
--- end

[a changed data file selects nothing instead of everything]
file scripts/mutate/affected.go
--- anchor
		default:
			return specs, nil
--- replace
		default:
			continue
--- end

[a change beside a mutated file in another package is not seen]
file scripts/mutate/affected.go
--- anchor
		if goDirs[path.Dir(path.Clean(m.file))] {
--- replace
		if false && goDirs[path.Dir(path.Clean(m.file))] {
--- end

[a change in a package the test binary is built from is not seen]
file scripts/mutate/affected.go
--- anchor
		if d.dirs[dir] {
--- replace
		if false && d.dirs[dir] {
--- end

[a test binary that runs go or git is not selected by every change]
file scripts/mutate/affected.go
--- anchor
	if d.wholeTree {
		return true, nil
	}
--- replace
	if false && d.wholeTree {
		return true, nil
	}
--- end

[the spec's tags are not passed to go list]
file scripts/mutate/affected.go
--- anchor
		args = append(args, "-tags="+tags)
--- replace
		args = append(args, "-tags="+tags[:0])
--- end

[a pkg that is not a directory is resolved through the import graph anyway]
file scripts/mutate/affected.go
--- anchor
	if _, ok := staticDir(c.root, pkg); !ok {
--- replace
	if _, ok := staticDir(c.root, pkg); false && !ok {
--- end

[a package whose imports fail to load is trusted]
file scripts/mutate/affected.go
--- anchor
		if p.Error != nil || len(p.DepsErrors) > 0 {
--- replace
		if false && (p.Error != nil || len(p.DepsErrors) > 0) {
--- end

[the standard library counts as this module's packages]
file scripts/mutate/affected.go
--- anchor
		if p.Standard || p.Module == nil || !p.Module.Main || p.Dir == "" {
--- replace
		if p.Dir == "" {
--- end

[the package's test variants, which hold its _test.go files, are not scanned]
file scripts/mutate/affected.go
--- anchor
		if p.Standard || p.Module == nil || !p.Module.Main || p.Dir == "" {
--- replace
		if p.Standard || strings.Contains(p.ImportPath, " [") || p.Module == nil || !p.Module.Main || p.Dir == "" {
--- end

[a file that runs git is not recognised]
file scripts/mutate/affected.go
--- anchor
(name == "go" || name == "git")
--- replace
(name == "go")
--- end

[CommandContext's program is read from its context argument]
file scripts/mutate/affected.go
--- anchor
		case "CommandContext":
			argIdx = 1
--- replace
		case "CommandContext":
			argIdx = 0
--- end
