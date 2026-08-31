package queue

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// liveJob and liveJobs are the in-package test replacements for the deleted
// Queue.Get and Queue.List.
//
// Those two were the only exits by which a live *Job — one aliasing the
// queue's own storage — could leave this package, and #463 deleted them so
// that "a Job is safe to read outside the queue lock" is true by construction
// rather than by convention. Every production reader already went through
// Snapshot/SnapshotJob, which deep-copy JobProgress under q.mu; the doors had
// no production caller at all, in this package or any other.
//
// These live in a _test.go file, so they are compiled only into the test
// binary and re-open nothing for production. They are also not a weakening of
// the guarantee even in principle: a test in package queue can already reach
// q.byID directly, so the door they provide is one the caller stands inside.
// They exist to keep the (*Job, error) shape those tests were written around,
// not to grant access the tests lacked.
//
// A test in another package must use Snapshot or SnapshotJob. That is the
// distinction the deletion is about, and there is deliberately no exported
// equivalent of these.
func (q *Queue) liveJob(id string) (*Job, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	job, ok := q.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return job, nil
}

// liveJobs returns the queue's jobs in order, aliasing its storage.
func (q *Queue) liveJobs() []*Job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]*Job, len(q.jobs))
	copy(out, q.jobs)
	return out
}

// TestNoExportedDoorHandsOutALiveJob enumerates, from source, every exported
// method on *Queue that returns a *Job, and requires each to return a clone.
//
// This is the enforcement for #463. The invariant it pins — no live *Job
// aliasing queue storage leaves this package — is what makes it safe for
// external callers to read a Job's contents without the queue lock, which in
// turn is why Progress() needs no lock and JobProgress's 28 accessors need no
// lock. Deleting Get and List established it; nothing but this test keeps it.
//
// It is a test rather than a comment because the population is machine
// enumerable, and because the failure mode of a comment here is silence: a
// future exported method returning q.byID[id] would reintroduce the hazard
// with no signal at all. AGENTS.md Rule 4: "Where the population is
// enumerable by a machine, write the test instead."
func TestNoExportedDoorHandsOutALiveJob(t *testing.T) {
	t.Parallel()

	// The only sanctioned doors. Both route through cloneJob under q.mu,
	// which deep-copies the manifest pointer's referent and JobProgress.
	cloning := map[string]bool{"Snapshot": true, "SnapshotJob": true}

	fset := token.NewFileSet()
	files := parseProductionFiles(t, fset)

	var found []string
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			if !isQueueReceiver(fn.Recv) {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if !returnsJob(res.Type) {
					continue
				}
				if !cloning[fn.Name.Name] {
					found = append(found, fmt.Sprintf("%s (%s:%d)",
						fn.Name.Name, name, fset.Position(fn.Pos()).Line))
				}
			}
		}
	}

	sort.Strings(found)
	if len(found) != 0 {
		t.Errorf("exported *Queue methods returning a *Job that is not a clone: %v\n"+
			"Every *Job leaving this package must be a snapshot — see Progress's doc "+
			"comment for what a live one exposes, and #463 for why Get and List were "+
			"deleted. If this method must exist, route it through cloneJob under q.mu "+
			"and add it to the map above.", found)
	}

	// The map must not go stale in the other direction: a door listed here
	// that no longer exists would silently widen what the test permits.
	for name := range cloning {
		if !hasExportedQueueMethod(files, name) {
			t.Errorf("%q is listed as a sanctioned cloning door but no longer exists", name)
		}
	}
}

func isQueueReceiver(recv *ast.FieldList) bool {
	if len(recv.List) == 0 {
		return false
	}
	star, ok := recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Queue"
}

// returnsJob reports whether a result type is *Job or []*Job.
func returnsJob(t ast.Expr) bool {
	switch e := t.(type) {
	case *ast.StarExpr:
		id, ok := e.X.(*ast.Ident)
		return ok && id.Name == "Job"
	case *ast.ArrayType:
		return returnsJob(e.Elt)
	}
	return false
}

func hasExportedQueueMethod(files map[string]*ast.File, name string) bool {
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv != nil && isQueueReceiver(fn.Recv) && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// parseProductionFiles parses this package's non-test .go files, keyed by
// base name.
//
// go/parser.ParseDir and go/ast.Package would be the obvious tools and are
// both deprecated; x/tools/go/packages is the sanctioned replacement but is a
// heavier dependency than a directory listing needs. Reading the directory and
// parsing each file keeps this to the standard library.
func parseProductionFiles(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/queue: %v", err)
	}
	out := make(map[string]*ast.File)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		out[n] = f
	}
	if len(out) == 0 {
		t.Fatal("parsed no production files; the walk is not looking where it thinks")
	}
	return out
}

// TestReturnsJob_SeesASliceOfLiveJobs pins the slice arm of returnsJob.
//
// The arm is not reachable through TestNoExportedDoorHandsOutALiveJob against
// the real package: the only exported method returning []*Job is Snapshot,
// which is allow-listed, so deleting the arm changes nothing there. A future
// `func (q *Queue) Pending() []*Job` handing out live pointers is exactly the
// regression this whole file exists to stop, and without this test the check
// that would catch it is itself unchecked.
func TestReturnsJob_SeesASliceOfLiveJobs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want bool
	}{
		{"func (q *Queue) Pending() []*Job { return nil }", true},
		{"func (q *Queue) One() *Job { return nil }", true},
		{"func (q *Queue) Nested() [][]*Job { return nil }", true},
		{"func (q *Queue) Len() int { return 0 }", false},
		{"func (q *Queue) Names() []string { return nil }", false},
		{"func (q *Queue) Err() error { return nil }", false},
	}

	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			file, err := parser.ParseFile(token.NewFileSet(), "x.go",
				"package queue\ntype Job struct{}\ntype Queue struct{}\n"+tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got bool
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Type.Results == nil {
					continue
				}
				for _, res := range fn.Type.Results.List {
					if returnsJob(res.Type) {
						got = true
					}
				}
			}
			if got != tc.want {
				t.Errorf("returnsJob = %v, want %v", got, tc.want)
			}
		})
	}
}
