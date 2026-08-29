package sched

import (
	"go/ast"
	"go/parser"
	"go/token"
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
// of q.mu's nine lockers takes it that way today. It does not attempt to
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
	// The nine names doc.go, queue.go and pool.go's comments state, sorted —
	// spelled out explicitly rather than derived, since a silently-wrong
	// `want` would defeat the whole point of this test.
	want := []string{"Advance", "Cancel", "Park", "Pause", "Paused", "Render", "Resume", "Retry", "Settle"}
	if !slices.Equal(got, want) {
		t.Errorf("methods calling q.mu.Lock() = %v, want %v\n\n"+
			"internal/sched/queue.go's own mu comment, internal/sched/pool.go's "+
			"leasePool comment, and internal/job/job.go's Snapshot comment all "+
			"claim these nine methods, and only these nine, take q.mu. If a "+
			"different set is correct, update all three comments AND this list "+
			"together — a comment that still names the old nine once a tenth "+
			"exists (or one is removed/renamed) is worse than no comment at all.",
			got, want)
	}
}
