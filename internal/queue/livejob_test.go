package queue

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// liveJob and liveJobs are the in-package test replacements for the deleted
// Queue.Get and Queue.List.
//
// Those two were the exits by which a live *Job — one aliasing the queue's
// own storage — could leave this package, and #463 deleted them so that "a
// Job is safe to read outside the queue lock" is true by construction rather
// than by convention.
//
// The enumeration behind that claim is not a grep and should not be dressed
// as one: post-deletion any grep for the deleted names trivially returns
// zero. What was actually checked, before the deletion, was that every one of
// the 22 external `.Progress()` call sites reached its Job through
// Snapshot/SnapshotJob, and that deleting both methods left `go build ./...`
// clean — so neither had a production caller in this package or any other.
// TestNoExportedDoorHandsOutALiveJob below is what keeps it true from here,
// and it enumerates from source on every run.
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
	return slices.Clone(q.jobs)
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
	// which deep-copies JobProgress and shares the Manifest pointer by
	// reference — Manifest is immutable after construction, so sharing it is
	// correct and cloning it would be a needless copy per snapshot. See
	// cloneJob's own doc comment, which states the same split.
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
					// One entry per method. A multi-result door such as
					// `ActivePair() (*Job, *Job)` would otherwise report the
					// same function and line once per result.
					break
				}
			}
		}
	}

	slices.Sort(found)
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
		if !hasExportedJobDoor(files, name) {
			t.Errorf("%q is listed as a sanctioned cloning door but is no longer an "+
				"exported *Queue method returning a Job", name)
		}
	}
}

// isQueueReceiver reports whether a method is declared on Queue, by pointer
// or by value.
//
// A value receiver is not a safe exclusion: Queue's job storage is `jobs
// []*Job` and `byID map[string]*Job`, both reference types, so a copy of the
// struct still points at the same live Jobs. `func (q Queue) Pending() *Job`
// would hand out queue-owned memory exactly as the deleted Get did, and
// matching only *ast.StarExpr would skip it silently.
func isQueueReceiver(recv *ast.FieldList) bool {
	if len(recv.List) == 0 {
		return false
	}
	return isQueueType(recv.List[0].Type)
}

func isQueueType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.StarExpr: // *Queue
		return isQueueType(t.X)
	case *ast.ParenExpr: // (*Queue)
		return isQueueType(t.X)
	case *ast.IndexExpr: // Queue[T], were it ever generic
		return isQueueType(t.X)
	case *ast.IndexListExpr:
		return isQueueType(t.X)
	case *ast.Ident:
		return t.Name == "Queue"
	}
	return false
}

// returnsJob reports whether a result type can carry a *Job out of the
// package, through any container rather than only a pointer or a slice.
//
// The containers matter as much as the bare pointer: `iter.Seq[*Job]`, `chan
// *Job`, `map[string]*Job` and a returned `func() *Job` all hand the caller
// the same aliased memory, and a check that recognised only *Job and []*Job
// would wave every one of them through. Go 1.23's range-over-func makes the
// iterator form a natural thing for a future author to reach for, which is
// exactly why it is enumerated here rather than left to be noticed.
func returnsJob(t ast.Expr) bool {
	switch e := t.(type) {
	case *ast.StarExpr: // *Job
		id, ok := e.X.(*ast.Ident)
		return ok && id.Name == "Job"
	case *ast.ArrayType: // []*Job, [N]*Job
		return returnsJob(e.Elt)
	case *ast.Ellipsis: // ...*Job
		return returnsJob(e.Elt)
	case *ast.ChanType: // chan *Job, <-chan *Job
		return returnsJob(e.Value)
	case *ast.MapType: // map[*Job]T and map[T]*Job
		return returnsJob(e.Key) || returnsJob(e.Value)
	case *ast.ParenExpr:
		return returnsJob(e.X)
	case *ast.IndexExpr: // iter.Seq[*Job]
		return returnsJob(e.Index)
	case *ast.IndexListExpr: // iter.Seq2[K, *Job]
		if slices.ContainsFunc(e.Indices, returnsJob) {
			return true
		}
	case *ast.FuncType: // func() *Job, func(*Job)
		for _, l := range fieldLists(e.Params, e.Results) {
			for _, f := range l.List {
				if returnsJob(f.Type) {
					return true
				}
			}
		}
	}
	return false
}

func fieldLists(ls ...*ast.FieldList) []*ast.FieldList {
	var out []*ast.FieldList
	for _, l := range ls {
		if l != nil {
			out = append(out, l)
		}
	}
	return out
}

// hasExportedJobDoor reports whether name is still an exported *Queue method
// that returns a Job.
//
// Checking the name alone was not enough: if Snapshot were refactored to
// return something other than a Job, the allow-list entry would keep matching
// and would go on excusing a name that no longer describes a door. The
// staleness check has to test the same property the allow-list grants an
// exemption from.
func hasExportedJobDoor(files map[string]*ast.File, name string) bool {
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != name ||
				!isQueueReceiver(fn.Recv) || fn.Type.Results == nil {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if returnsJob(res.Type) {
					return true
				}
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
	files, err := parseQueueFiles(fset, ".")
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// parseQueueFiles is parseProductionFiles' body, returning its error rather
// than failing the test, so the empty-directory guard below can be exercised
// directly. Reached from the package directory the guard never fires, which
// makes it exactly the kind of defensive branch that goes unchecked.
func parseQueueFiles(fset *token.FileSet, dir string) (map[string]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := make(map[string]*ast.File)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", n, err)
		}
		// A .go file in this directory is not automatically part of this
		// package — a `//go:build ignore` helper declares its own package
		// name, and judging it as queue code would be wrong in both
		// directions.
		if f.Name == nil || f.Name.Name != "queue" {
			continue
		}
		out[n] = f
	}
	// A guard, not a formality: `go test` sets the working directory to the
	// package directory, but a precompiled binary (`go test -c`) run from
	// elsewhere would read a directory with no queue sources and every
	// assertion below would pass vacuously.
	if len(out) == 0 {
		return nil, fmt.Errorf("parsed no queue package files in %s; the working "+
			"directory is not internal/queue, so the caller would pass without "+
			"checking anything", dir)
	}
	return out, nil
}

// TestReturnsJob_SeesASliceOfLiveJobs pins the slice arm of returnsJob.
//
// The arm is not reachable through TestNoExportedDoorHandsOutALiveJob against
// the real package: the only exported method returning []*Job is Snapshot,
// which is allow-listed, so deleting the arm changes nothing there. A future
// `func (q *Queue) Pending() []*Job` handing out live pointers is exactly the
// regression this whole file exists to stop, and without this test the check
// that would catch it is itself unchecked.
func TestIsQueueReceiver_SeesValueAndParenthesizedForms(t *testing.T) {
	t.Parallel()

	// A value receiver is the bypass worth naming: Queue's storage is
	// `jobs []*Job` and `byID map[string]*Job`, both reference types, so a
	// copy of the struct still points at the same live Jobs.
	cases := []struct {
		src  string
		want bool
	}{
		{"func (q *Queue) M() {}", true},
		{"func (q Queue) M() {}", true},
		{"func (q (*Queue)) M() {}", true},
		{"func (j *Job) M() {}", false},
		{"func (p *JobProgress) M() {}", false},
	}

	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			file, err := parser.ParseFile(token.NewFileSet(), "x.go",
				"package queue\n"+
					"type Job struct{}\ntype JobProgress struct{}\n"+
					"type Queue struct{}\n"+tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got bool
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil {
					got = isQueueReceiver(fn.Recv)
				}
			}
			if got != tc.want {
				t.Errorf("isQueueReceiver = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReturnsJob_SeesALiveJobThroughAnyContainer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want bool
	}{
		{"func (q *Queue) Pending() []*Job { return nil }", true},
		{"func (q *Queue) One() *Job { return nil }", true},
		{"func (q *Queue) Nested() [][]*Job { return nil }", true},
		{"func (q *Queue) Arr() [4]*Job { return [4]*Job{} }", true},
		// Go 1.23 range-over-func: the form a future author is most likely
		// to reach for, and the one a *Job/[]*Job check waves straight through.
		{"func (q *Queue) All() iter.Seq[*Job] { return nil }", true},
		{"func (q *Queue) Pairs() iter.Seq2[string, *Job] { return nil }", true},
		{"func (q *Queue) Stream() <-chan *Job { return nil }", true},
		{"func (q *Queue) ByID() map[string]*Job { return nil }", true},
		{"func (q *Queue) Keyed() map[*Job]bool { return nil }", true},
		{"func (q *Queue) Next() func() *Job { return nil }", true},
		{"func (q *Queue) Each() func(*Job) { return nil }", true},
		{"func (q *Queue) Len() int { return 0 }", false},
		{"func (q *Queue) Names() []string { return nil }", false},
		{"func (q *Queue) Err() error { return nil }", false},
		{"func (q *Queue) Seen() map[string]bool { return nil }", false},
		{"func (q *Queue) Done() <-chan struct{} { return nil }", false},
	}

	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			file, err := parser.ParseFile(token.NewFileSet(), "x.go",
				"package queue\nimport \"iter\"\nvar _ iter.Seq[int]\ntype Job struct{}\ntype Queue struct{}\n"+tc.src, 0)
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

// TestParseQueueFiles_RefusesADirectoryWithNoQueueSources pins the guard that
// stops TestNoExportedDoorHandsOutALiveJob passing vacuously.
//
// Run from the package directory the guard never fires, so a mutation
// disabling it SURVIVED until this test existed. Its failure mode is the one
// worth spending a test on: a precompiled binary (`go test -c`) run from
// elsewhere would parse no queue sources, find no offending doors, and report
// the architectural invariant as upheld without having read a line of it.
func TestParseQueueFiles_RefusesADirectoryWithNoQueueSources(t *testing.T) {
	t.Parallel()

	if _, err := parseQueueFiles(token.NewFileSet(), t.TempDir()); err == nil {
		t.Fatal("parseQueueFiles accepted a directory with no queue sources; " +
			"the caller would then check nothing and pass")
	}

	// A .go file that is not part of this package must not count either.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.go"),
		[]byte("//go:build ignore\n\npackage main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := parseQueueFiles(token.NewFileSet(), dir); err == nil {
		t.Error("a build-ignored package main file was counted as queue source")
	}

	if files, err := parseQueueFiles(token.NewFileSet(), "."); err != nil || len(files) == 0 {
		t.Errorf("parseQueueFiles(\".\") = %d files, %v; want the real package", len(files), err)
	}
}

// TestHasExportedJobDoor_ChecksTheReturnTypeNotOnlyTheName pins the staleness
// half of the allow-list.
//
// Against the real package the check cannot be observed: Snapshot and
// SnapshotJob both do return Jobs, so removing the return-type test changes
// nothing and the mutation SURVIVED. It matters only in the state it exists to
// catch — a sanctioned name that has been refactored to return something else,
// where matching on the name alone would go on excusing a door that is no
// longer one.
func TestHasExportedJobDoor_ChecksTheReturnTypeNotOnlyTheName(t *testing.T) {
	t.Parallel()

	parse := func(src string) map[string]*ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "x.go",
			"package queue\ntype Job struct{}\ntype Queue struct{}\n"+src, 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return map[string]*ast.File{"x.go": f}
	}

	if !hasExportedJobDoor(parse("func (q *Queue) Snapshot() []*Job { return nil }"), "Snapshot") {
		t.Error("a real door returning []*Job was not recognised")
	}
	if hasExportedJobDoor(parse("func (q *Queue) Snapshot() []string { return nil }"), "Snapshot") {
		t.Error("a method that no longer returns a Job still counted as a door; " +
			"the allow-list entry would silently keep excusing the name")
	}
	if hasExportedJobDoor(parse("func (q *Queue) Other() *Job { return nil }"), "Snapshot") {
		t.Error("a differently named method satisfied the Snapshot entry")
	}
}
