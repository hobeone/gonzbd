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

// productionCallers returns, as "file:function", every function in this
// package's non-test files that calls a method with one of names.
func productionCallers(t *testing.T, names ...string) []string {
	t.Helper()
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
				if sel, ok := n.(*ast.SelectorExpr); ok && slices.Contains(names, sel.Sel.Name) {
					sites = append(sites, name+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	slices.Sort(sites)
	return slices.Compact(sites)
}

// TestJobTransitions_LockSites pins the set of production functions that take
// a job's transition lock: every call of a method named acquire or tryAcquire
// across this package's non-test files. A function added to the set is an
// actor the lock-order argument in jobTransitions has not been made for, and
// one dropped from it acts on a job's state unexcluded.
func TestJobTransitions_LockSites(t *testing.T) {
	want := []string{
		"app.go:MarkHistoryCompleted",
		"app.go:PruneHistory",
		"app.go:RemoveHistoryJob",
		"app.go:RemoveJob",
		"app.go:retryHistoryJob",
		"job_finalizer.go:persistAndCommit",
	}
	if sites := productionCallers(t, "acquire", "tryAcquire"); !slices.Equal(sites, want) {
		t.Errorf("functions taking a job's transition lock = %v, want %v", sites, want)
	}
}

// TestJobTransitions_FinalizingSites pins the finalizing record to one writer,
// the finalizer, and its one reader outside tryAcquire, retryHistoryJob,
// which checks it after its registration check and again before it
// registers. A second writer would refuse retries no finalizer excludes; a
// reader dropped would let a retry that took the lock first change state, or
// register, under a finalizer's teardown.
func TestJobTransitions_FinalizingSites(t *testing.T) {
	if sites, want := productionCallers(t, "beginFinalize"), []string{"job_finalizer.go:persistAndCommit"}; !slices.Equal(sites, want) {
		t.Errorf("functions writing the finalizing record = %v, want %v", sites, want)
	}
	if sites, want := productionCallers(t, "isFinalizing"), []string{"app.go:retryHistoryJob"}; !slices.Equal(sites, want) {
		t.Errorf("functions reading the finalizing record = %v, want %v", sites, want)
	}
}

// TestJobTransitions_RemovedSites pins the removal mark to one writer, and its
// three readers: the DirectUnpack start, the finalizer, and the
// post-processing hand-over. A second writer would make the finalizer skip a
// job no user removed, and a completed job would be filed nowhere.
func TestJobTransitions_RemovedSites(t *testing.T) {
	if sites, want := productionCallers(t, "markRemoved"), []string{"app.go:RemoveJob"}; !slices.Equal(sites, want) {
		t.Errorf("functions writing the removal mark = %v, want %v", sites, want)
	}
	if sites, want := productionCallers(t, "wasRemoved"), []string{
		"directunpack_orchestrator.go:maybeStart",
		"job_finalizer.go:persistAndCommit",
		"postproc_admission.go:beginHandOver",
	}; !slices.Equal(sites, want) {
		t.Errorf("functions reading the removal mark = %v, want %v", sites, want)
	}
}
