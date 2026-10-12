package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// listTests names every top-level Test function the spec's package declares
// — the set `go test -list '^Test'` prints — so a `run` alternative can be
// checked against it. Example, Benchmark and Fuzz functions are not listed: a
// spec's `run` line in this format only ever names tests.
//
// It reads declarations instead of asking `go test -list`, which compiles and
// links a test binary per package only to print names the source already
// holds. go/build picks the package's _test.go files under the spec's tags and
// the go command's own GOOS, GOARCH and CGO_ENABLED; go/parser finds the
// functions in them by the rule `go test` applies (see testFuncs).
//
// Two cases go to `go test -list` instead, because they need the go command's
// own interpretation: a pkg that is a pattern or an import path rather than a
// directory (staticDir), and an environment with GOFLAGS set, which can carry
// build tags of its own.
func listTests(root string, sp *spec) ([]string, error) {
	env, err := goEnvForListing()
	if err != nil {
		return nil, err
	}
	return listTestsIn(env, root, sp)
}

// listTestsIn is listTests under a given go environment.
func listTestsIn(env listingEnv, root string, sp *spec) ([]string, error) {
	dir, ok := staticDir(root, sp.pkg)
	if !ok || env.GOFLAGS != "" {
		return goListTests(root, sp)
	}
	return staticTests(env.context(sp.tags), dir)
}

// staticDir resolves a spec's pkg to the one directory it names, and reports
// false for anything the go command would expand or look up — a `...`
// pattern, an import path — which listTests leaves to `go test -list`.
func staticDir(root, pkg string) (string, bool) {
	if strings.Contains(pkg, "...") {
		return "", false
	}
	if pkg != "." && pkg != ".." && !strings.HasPrefix(pkg, "./") && !strings.HasPrefix(pkg, "../") {
		return "", false
	}
	return filepath.Join(root, pkg), true
}

// listingEnv is the part of the go command's environment that decides which
// files a build includes.
type listingEnv struct {
	GOOS, GOARCH, CGO_ENABLED, GOFLAGS string //nolint:revive // named as `go env -json` prints them
}

// context is build.Default with the go command's view of the target and the
// spec's tags. `-tags` accepts a comma- or space-separated list, so both
// separate here.
func (e listingEnv) context(tags string) build.Context {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH = e.GOOS, e.GOARCH
	ctx.CgoEnabled = e.CGO_ENABLED == "1"
	ctx.BuildTags = strings.FieldsFunc(tags, func(r rune) bool { return r == ',' || r == ' ' })
	return ctx
}

// goEnvForListing asks the go command, once per process, for the settings
// listingEnv holds. go/build's own defaults read only the process environment
// and miss a value set with `go env -w`, which `go test` would honour.
var goEnvForListing = sync.OnceValues(func() (listingEnv, error) {
	out, err := exec.Command("go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS").Output()
	if err != nil {
		return listingEnv{}, fmt.Errorf("go env: %w", err)
	}
	var e listingEnv
	if err := json.Unmarshal(out, &e); err != nil {
		return listingEnv{}, fmt.Errorf("go env: %w", err)
	}
	return e, nil
})

// staticTests lists the Test functions declared in dir's in-package and
// external test files under ctx.
func staticTests(ctx build.Context, dir string) ([]string, error) {
	p, err := ctx.ImportDir(dir, 0)
	if err != nil {
		// A NoGoError is also what `go test` reports, as a failure, for a
		// directory with nothing to build.
		return nil, fmt.Errorf("list tests in %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, f := range slices.Concat(p.TestGoFiles, p.XTestGoFiles) {
		file, err := parser.ParseFile(fset, filepath.Join(dir, f), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("list tests in %s: %w", dir, err)
		}
		found, err := testFuncs(file)
		if err != nil {
			return nil, fmt.Errorf("list tests in %s: %s: %w", dir, f, err)
		}
		names = append(names, found...)
	}
	slices.Sort(names)
	return names, nil
}

// testFuncs applies the rule cmd/go uses to collect a test binary's tests
// (cmd/go/internal/load/test.go, loadTestFuncs): a top-level function whose
// name is Test or Test followed by a character that is not lower case. TestMain
// taking *M is the binary's entry hook, not a test. A Test-named function with
// the wrong signature fails `go test` outright, so it is an error here too
// rather than a silently missing name.
func testFuncs(file *ast.File) ([]string, error) {
	var names []string
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		name := fn.Name.Name
		if name == "TestMain" && !takesPointerTo(fn, "T") {
			continue
		}
		if !isTestName(name) {
			continue
		}
		if !takesPointerTo(fn, "T") || fn.Type.TypeParams.NumFields() > 0 {
			return nil, errors.New("wrong signature for " + name + ", must be: func " + name + "(t *testing.T)")
		}
		names = append(names, name)
	}
	return names, nil
}

// isTestName is cmd/go's isTest(name, "Test"): `Testable` is not a test,
// `Test`, `TestX` and `Test_x` are.
func isTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// takesPointerTo is cmd/go's isTestFunc: one parameter of type *T or *x.T,
// and no results. Like cmd/go it cannot check that x is the testing package.
func takesPointerTo(fn *ast.FuncDecl, typ string) bool {
	params := fn.Type.Params.List
	if fn.Type.Results.NumFields() > 0 || len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	ptr, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch x := ptr.X.(type) {
	case *ast.Ident:
		return x.Name == typ
	case *ast.SelectorExpr:
		return x.Sel.Name == typ
	}
	return false
}

// listArgs builds the argv for `go test -list`, honouring the spec's build
// tags the same way testArgs does for the baseline run.
func listArgs(sp *spec) []string {
	args := []string{"test", "-list", "^Test"}
	if sp.tags != "" {
		args = append(args, "-tags="+sp.tags)
	}
	args = append(args, sp.pkg)
	return args
}

// goListTests is listTests by way of `go test -list`, for the cases the
// static reading leaves to the go command. It compiles the test binary.
func goListTests(root string, sp *spec) ([]string, error) {
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
	slices.Sort(names)
	return names, nil
}
