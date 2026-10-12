package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// affectedSpecs keeps the specs whose verdicts a change to changed can move.
// specs are repository-relative paths as discoverSpecs returns them. It may
// select a spec the change cannot move; it must not leave out one it can.
//
// A spec is selected when:
//
//   - the spec file itself changed;
//   - a changed .go file sits in the directory of a file one of its mutations
//     edits;
//   - a changed .go file sits in the directory of a package of this module
//     that the spec's test binary is built from (testDeps), which covers the
//     spec's own package, its _test.go files and everything it transitively
//     imports;
//   - its test binary runs `go` or `git` (testDeps' wholeTree), since such a
//     test can read any file in the checkout — every change selects it.
//
// Any other changed file — not .go and not a spec — selects every spec. Tests
// read files by path at runtime: fixtures under test/fixtures/, another
// package's testdata/, docs/job-lifecycle.md, ui/src/. None of those reads is
// visible in the import graph, so a data file is never attributed to a
// package; go.mod and go.sum, which move every dependency, fall here too.
//
// What it cannot see: a test that reads a .go file outside its own package's
// directory by a path built at runtime, without running go or git. Of the
// tests that parse source (`git grep -l 'parser.ParseFile' -- '*_test.go'`),
// each reads its own directory or lists the tree with git.
func affectedSpecs(root string, specs, changed []string) ([]string, error) {
	if len(changed) == 0 {
		return nil, nil
	}
	isSpec := make(map[string]bool, len(specs))
	for _, s := range specs {
		isSpec[path.Clean(s)] = true
	}
	changedSpec := map[string]bool{}
	goDirs := map[string]bool{}
	for _, c := range changed {
		switch {
		case isSpec[c]:
			changedSpec[c] = true
		case strings.HasSuffix(c, ".go"):
			goDirs[path.Dir(c)] = true
		default:
			return specs, nil
		}
	}

	deps := newDepCache(root)
	var out []string
	for _, s := range specs {
		if changedSpec[path.Clean(s)] {
			out = append(out, s)
			continue
		}
		sp, err := parseSpec(filepath.Join(root, s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
		hit, err := specTouched(sp, goDirs, deps)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
		if hit {
			out = append(out, s)
		}
	}
	return out, nil
}

// specTouched applies affectedSpecs' rules to a spec whose own file did not
// change.
func specTouched(sp *spec, goDirs map[string]bool, deps *depCache) (bool, error) {
	for _, m := range sp.mutations {
		if goDirs[path.Dir(path.Clean(m.file))] {
			return true, nil
		}
	}
	d, err := deps.get(sp.pkg, sp.tags)
	if err != nil {
		return false, err
	}
	if d.wholeTree {
		return true, nil
	}
	for dir := range goDirs {
		if d.dirs[dir] {
			return true, nil
		}
	}
	return false, nil
}

// testDeps is what a spec's test binary is built from, as far as this module
// is concerned.
type testDeps struct {
	// dirs holds the repository-relative directory of every package of this
	// module the test binary compiles.
	dirs map[string]bool
	// wholeTree is set when the binary can depend on any file in the
	// checkout: one of its files runs `go` or `git`, the package could not
	// be resolved to a directory, or one of its imports failed to load.
	wholeTree bool
}

// depCache computes testDeps once per (pkg, tags), and remembers which
// source files run `go` or `git` across all of them.
type depCache struct {
	root  string
	mu    sync.Mutex
	byKey map[[2]string]testDeps
	execs map[string]bool
}

func newDepCache(root string) *depCache {
	return &depCache{root: root, byKey: map[[2]string]testDeps{}, execs: map[string]bool{}}
}

// listedPackage is the part of `go list -json` testDeps reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *struct{ Main bool }
	GoFiles    []string
	CgoFiles   []string
	Error      *struct{ Err string }
	DepsErrors []*struct{ Err string }
}

func (c *depCache) get(pkg, tags string) (testDeps, error) {
	key := [2]string{pkg, tags}
	c.mu.Lock()
	d, ok := c.byKey[key]
	c.mu.Unlock()
	if ok {
		return d, nil
	}
	d, err := c.compute(pkg, tags)
	if err != nil {
		return testDeps{}, err
	}
	c.mu.Lock()
	c.byKey[key] = d
	c.mu.Unlock()
	return d, nil
}

// compute runs `go list -deps -test` for pkg under tags. It reads package
// metadata only; nothing is compiled.
func (c *depCache) compute(pkg, tags string) (testDeps, error) {
	if _, ok := staticDir(c.root, pkg); !ok {
		return testDeps{wholeTree: true}, nil
	}
	args := []string{"list", "-e", "-deps", "-test",
		"-json=ImportPath,Dir,Standard,Module,GoFiles,CgoFiles,Error,DepsErrors"}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...) //nolint:gosec // G204: argv comes from the operator's own spec, the same trust level as testArgs
	cmd.Dir = c.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return testDeps{}, fmt.Errorf("go list %s: %w\n%s", pkg, err, indent(stderr.String()))
	}

	d := testDeps{dirs: map[string]bool{}}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return testDeps{}, fmt.Errorf("go list %s: %w", pkg, err)
		}
		if p.Error != nil || len(p.DepsErrors) > 0 {
			d.wholeTree = true
		}
		if p.Standard || p.Module == nil || !p.Module.Main || p.Dir == "" {
			continue
		}
		rel, err := filepath.Rel(c.root, p.Dir)
		if err != nil {
			return testDeps{}, err
		}
		d.dirs[filepath.ToSlash(rel)] = true

		// GoFiles is what each listed package compiles: the package under
		// test appears as its test variants, whose GoFiles carry its
		// _test.go files, while a dependency's own tests are not built.
		for _, f := range slices.Concat(p.GoFiles, p.CgoFiles) {
			if filepath.IsAbs(f) {
				// The generated test main lives in the build cache, not
				// the checkout.
				continue
			}
			runs, err := c.runsGoOrGit(filepath.Join(p.Dir, f))
			if err != nil {
				return testDeps{}, err
			}
			if runs {
				d.wholeTree = true
			}
		}
	}
	return d, nil
}

// runsGoOrGit reports whether file calls exec.Command or exec.CommandContext
// with "go" or "git" as the program, cached per file.
func (c *depCache) runsGoOrGit(file string) (bool, error) {
	c.mu.Lock()
	v, ok := c.execs[file]
	c.mu.Unlock()
	if ok {
		return v, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return false, err
	}
	v = callsGoOrGit(f)
	c.mu.Lock()
	c.execs[file] = v
	c.mu.Unlock()
	return v, nil
}

// callsGoOrGit is runsGoOrGit's AST walk. The program argument must be a
// string literal to be recognised. Of the calls whose program is not one
// (`git grep -n -E 'exec\.Command(Context)?\(' -- '*.go'`), only
// scripts/check_citations/main.go's grep runner can start git, and that file
// also calls git by literal.
func callsGoOrGit(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		argIdx := -1
		switch sel.Sel.Name {
		case "Command":
			argIdx = 0
		case "CommandContext":
			argIdx = 1
		}
		if argIdx < 0 || len(call.Args) <= argIdx {
			return true
		}
		lit, ok := call.Args[argIdx].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if name, err := strconv.Unquote(lit.Value); err == nil && (name == "go" || name == "git") {
			found = true
		}
		return !found
	})
	return found
}
