package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestJobTransitions_LockSites pins the set of production functions that take
// a job's transition lock: every call of a method named acquire or tryAcquire
// across this package's non-test files. A function added to the set is an
// actor the lock-order argument in jobTransitions has not been made for, and
// one dropped from it acts on a job's state unexcluded.
func TestJobTransitions_LockSites(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	var sites []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "acquire" || sel.Sel.Name == "tryAcquire") {
					sites = append(sites, name+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	slices.Sort(sites)
	sites = slices.Compact(sites)

	want := []string{
		"app.go:MarkHistoryCompleted",
		"app.go:PruneHistory",
		"app.go:RemoveHistoryJob",
		"app.go:RemoveJob",
		"app.go:RetryHistoryJob",
		"job_finalizer.go:persistAndCommit",
	}
	if !slices.Equal(sites, want) {
		t.Errorf("functions taking a job's transition lock = %v, want %v", sites, want)
	}
}
