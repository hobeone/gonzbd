package sched

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scanQMuLockers parses this package's non-test sources and returns the
// sorted, deduplicated names of the *Queue methods whose body calls
// q.mu.Lock() directly — a `q.mu.Lock()` call expression, matched on the
// literal selector chain rather than resolved types, exactly as
// internal/job/writer_enumeration_test.go's scanWriters matches field
// writes on the selector name alone.
//
// It walks each *ast.FuncDecl's whole body rather than only its top level,
// so a lock taken inside a nested closure would still be found — though none
// of q.mu's ten lockers takes it that way today. It does not attempt to
// verify the receiver is actually named q; every *Queue method in this
// package uses that name (queue.go's own comment on mu already relies on
// this), and a differently-named receiver calling q.mu.Lock() literally
// would not compile, since q would be undefined.
func scanQMuLockers(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	var lockers []string
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++

		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv == nil {
				continue
			}
			// Only methods on *Queue are candidates; a package-level func or a
			// method on a different receiver type (leasePool, slotPool) is not
			// part of this population.
			isQueueMethod := false
			for _, f := range fd.Recv.List {
				star, ok := f.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "Queue" {
					isQueueMethod = true
				}
			}
			if !isQueueMethod {
				continue
			}

			locks := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// q.mu.Lock() parses as CallExpr{Fun: SelectorExpr{X:
				// SelectorExpr{X: Ident("q"), Sel: Ident("mu")}, Sel:
				// Ident("Lock")}}.
				outer, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || outer.Sel.Name != "Lock" {
					return true
				}
				inner, ok := outer.X.(*ast.SelectorExpr)
				if !ok || inner.Sel.Name != "mu" {
					return true
				}
				if ident, ok := inner.X.(*ast.Ident); ok && ident.Name == "q" {
					locks = true
				}
				return true
			})
			if locks {
				lockers = append(lockers, fd.Name.Name)
			}
		}
	}

	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}
	if len(lockers) == 0 {
		t.Fatal("scan found zero q.mu lockers; an empty result reads identically to " +
			"\"nothing locks it\" and means the scan itself is broken, not that the " +
			"population is empty")
	}

	slices.Sort(lockers)
	return slices.Compact(lockers)
}

// flatParams flattens a *ast.FieldList (receiver or parameter list) into an
// ordered slice of (name, type) pairs, one per declared identifier — Go
// allows a single Field to carry several names sharing one type
// (`func f(a, b string)`), so a naive one-pair-per-Field walk would
// under-count and misalign a later positional match against call arguments.
// An unnamed parameter (no Names) contributes one blank-named entry so
// position is still preserved.
func flatParams(fl *ast.FieldList) []struct {
	name string
	typ  ast.Expr
} {
	var out []struct {
		name string
		typ  ast.Expr
	}
	if fl == nil {
		return out
	}
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			out = append(out, struct {
				name string
				typ  ast.Expr
			}{"", f.Type})
			continue
		}
		for _, n := range f.Names {
			out = append(out, struct {
				name string
				typ  ast.Expr
			}{n.Name, f.Type})
		}
	}
	return out
}

// isJobJobPtr reports whether e is the type expression `*job.Job` — the only
// type this package ever hands a *Queue method that later reaches a *job.Job
// method call.
func isJobJobPtr(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "job" && sel.Sel.Name == "Job"
}

// isJobJobSlice reports whether e is the type expression `[]*job.Job` —
// RenderAll's js parameter is the only site of it in this package's non-test
// sources.
func isJobJobSlice(e ast.Expr) bool {
	arr, ok := e.(*ast.ArrayType)
	if !ok || arr.Len != nil { // Len != nil is a fixed-size array, not a slice
		return false
	}
	return isJobJobPtr(arr.Elt)
}

// queueMethodFuncs parses this package's non-test sources and returns every
// *Queue method (exported or not) and every receiver-less package function,
// keyed by name. leasePool and slotPool methods are deliberately excluded:
// neither type is ever handed a *job.Job (their signatures take only
// job.LeaseID, string ids, or nothing), so they can never reach a *job.Job
// method call, and including them would let slotPool.holds collide in this
// map with Queue's own same-named holds (pool.go:125 and queue.go:192) —
// admitting a callee that can never carry a job argument does no harm to the
// reachability walk below, but the name collision would make map-building
// order-dependent, so the exclusion is the simpler and exactly-sufficient
// fix.
func queueMethodFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	funcs := make(map[string]*ast.FuncDecl)
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Recv == nil {
				funcs[fd.Name.Name] = fd
				continue
			}
			for _, f := range fd.Recv.List {
				star, ok := f.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "Queue" {
					funcs[fd.Name.Name] = fd
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}
	return funcs
}

// reachesJobMethod reports whether fd's body — walked through calls into
// other *Queue methods and package functions in funcs, but never past a
// visited name twice — contains a call expression whose receiver is a
// variable known (via jobVars) to hold a *job.Job. jobVars starts as fd's own
// parameter names typed *job.Job and is recomputed at each recursive step by
// positionally matching the call's argument identifiers against the callee's
// own *job.Job-typed parameters, so a job pointer threaded through several
// layers of unexported helpers (Cancel -> finishCancel, Park -> parkLocked,
// Advance -> grantFor/moveTo) is still tracked to wherever it is finally used.
// jobSliceVars is the same idea for a []*job.Job parameter (RenderAll's js):
// a `for _, j := range js` binds j as a *job.Job for the rest of the walk,
// found via *ast.RangeStmt rather than a call argument.
//
// This is name-and-position matching, not a type checker: it cannot see
// through a reassignment (`j2 := j; j2.Foo()`) or a value stored in a
// struct field and read back. Nothing in this package's *Queue methods does
// either as of this writing, which is a claim about the CURRENT bodies, not a
// guarantee the pattern will always suffice — a future body written that way
// would need this helper extended, not just trusted.
func reachesJobMethod(fd *ast.FuncDecl, jobVars map[string]bool, jobSliceVars map[string]bool, funcs map[string]*ast.FuncDecl, visited map[string]bool) bool {
	if fd.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		if rs, ok := n.(*ast.RangeStmt); ok {
			if xIdent, ok := rs.X.(*ast.Ident); ok && jobSliceVars[xIdent.Name] {
				if valIdent, ok := rs.Value.(*ast.Ident); ok {
					jobVars[valIdent.Name] = true
				}
			}
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var calleeName string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			ident, ok := fn.X.(*ast.Ident)
			if !ok {
				return true
			}
			if jobVars[ident.Name] {
				found = true
				return false
			}
			calleeName = fn.Sel.Name
		case *ast.Ident:
			calleeName = fn.Name
		default:
			return true
		}
		callee, ok := funcs[calleeName]
		if !ok || visited[calleeName] {
			return true
		}
		params := flatParams(callee.Type.Params)
		childVars := make(map[string]bool)
		for i, p := range params {
			if !isJobJobPtr(p.typ) || p.name == "" {
				continue
			}
			if i >= len(call.Args) {
				continue
			}
			argIdent, ok := call.Args[i].(*ast.Ident)
			if ok && jobVars[argIdent.Name] {
				childVars[p.name] = true
			}
		}
		if len(childVars) == 0 {
			return true
		}
		nextVisited := make(map[string]bool, len(visited)+1)
		maps.Copy(nextVisited, visited)
		nextVisited[calleeName] = true
		// No *Queue method or package function this package's callees reach
		// takes a []*job.Job: `grep -n '\[\]\*job\.Job' internal/sched/*.go |
		// grep -v _test.go` finds exactly one line, RenderAll's own
		// signature, and RenderAll does not pass js itself to any callee — it
		// ranges over it and passes one *job.Job at a time — so an empty map
		// here costs nothing today. If a future helper takes []*job.Job, this
		// recursion would need to thread jobSliceVars through the same way
		// childVars is threaded above.
		if reachesJobMethod(callee, childVars, nil, funcs, nextVisited) {
			found = true
			return false
		}
		return true
	})
	return found
}

// doorsReachingJobMethod reports, for every exported *Queue door, whether its
// body — transitively, through this package's own unexported helpers —
// reaches a call on a *job.Job value. It is the machine check
// internal/job/job.go's seven-of-ten prose split names as its own
// enforcement: see TestQueueDoorsReachingJob_MatchTheEnumerationStatedInProse
// below.
func doorsReachingJobMethod(t *testing.T) map[string]bool {
	t.Helper()
	funcs := queueMethodFuncs(t)
	got := make(map[string]bool)
	for name, fd := range funcs {
		if !fd.Name.IsExported() || fd.Recv == nil {
			continue
		}
		jobVars := make(map[string]bool)
		jobSliceVars := make(map[string]bool)
		for _, p := range flatParams(fd.Type.Params) {
			switch {
			case isJobJobPtr(p.typ) && p.name != "":
				jobVars[p.name] = true
			case isJobJobSlice(p.typ) && p.name != "":
				// RenderAll's js is []*job.Job, not *job.Job — reachesJobMethod
				// promotes its range variable into jobVars when it finds a
				// `for _, j := range js` RangeStmt.
				jobSliceVars[p.name] = true
			}
		}
		got[name] = reachesJobMethod(fd, jobVars, jobSliceVars, funcs, map[string]bool{name: true})
	}
	return got
}

// TestQueueMuLockers_MatchTheEnumerationStatedInProse enforces the count AND
// the names of q.mu's lockers — the fact stated in prose at three sites
// (doc.go, queue.go, pool.go) and, per the final Half B2.1 review, broken by
// three of six tasks along the way despite check_citations validating the
// COUNT at queue.go:39 and internal/job/job.go:132: none of the three prose
// sites' NAMES were ever checked by a gate, so renaming Park back to park
// would have left every gate green while making all three comments wrong.
//
// This is the enforcement all three sites point at, rather than a fourth
// hand-maintained copy of the same list.
func TestQueueMuLockers_MatchTheEnumerationStatedInProse(t *testing.T) {
	got := scanQMuLockers(t)
	// The ten names doc.go, queue.go and pool.go's comments state, sorted —
	// spelled out explicitly rather than derived, since a silently-wrong
	// `want` would defeat the whole point of this test.
	want := []string{"Advance", "Cancel", "Park", "Pause", "Paused", "Render", "RenderAll", "Resume", "Retry", "Settle"}
	if !slices.Equal(got, want) {
		t.Errorf("methods calling q.mu.Lock() = %v, want %v\n\n"+
			"internal/sched/queue.go's own mu comment, internal/sched/pool.go's "+
			"leasePool comment, and internal/job/job.go's Snapshot comment all "+
			"claim these ten methods, and only these ten, take q.mu. If a "+
			"different set is correct, update all three comments AND this list "+
			"together — a comment that still names the old ten once an eleventh "+
			"exists (or one is removed/renamed) is worse than no comment at all.",
			got, want)
	}
}

// TestQueueDoorsReachingJob_MatchTheEnumerationStatedInProse pins the
// seven-of-ten split that internal/job/job.go states in prose. That comment
// was explicitly "a reviewed property, not a machine-checked one", and adding
// RenderAll moved it from six-of-nine to seven-of-ten — a change no gate in
// this repository would have caught, since
// TestQueueMuLockers_MatchTheEnumerationStatedInProse asserts locker NAMES
// only.
func TestQueueDoorsReachingJob_MatchTheEnumerationStatedInProse(t *testing.T) {
	want := map[string]bool{
		"Cancel": true, "Park": true, "Retry": true, "Advance": true,
		"Settle": true, "Render": true, "RenderAll": true,
		"Pause": false, "Resume": false, "Paused": false,
	}
	got := doorsReachingJobMethod(t) // set of exported doors whose body reaches a *job.Job method
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("door %s reaches *job.Job = %v, want %v — update internal/job/job.go's prose split", name, got[name], expected)
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %d exported doors, prose enumerates %d", len(got), len(want))
	}
}
