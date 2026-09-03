package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestDispatchNamesNoManifestType is the residency boundary, demoted from a
// compiler guarantee to a test one when Manifest moved into internal/job (Task
// 1). Before the move, dispatch could not name the manifest type at all: it
// lived in internal/queue, which this package may not import. It now lives in
// internal/job, which this package DOES import, so this test is what stands
// between the dispatcher and the manifest's contents — see the two checks
// below for exactly what that covers and does not.
//
// The dispatcher decides WHEN a job is resident and delegates WHAT to load to
// Residency; naming the manifest type here, or reading manifest contents
// through a *job.Job it already holds, is how that separation gets lost.
//
// It parses this package's own non-test source rather than grepping, so a
// reference in a comment or a string does not trip it and a reference in a
// type does. It sees only THIS package: a violation in internal/dispatch/store
// is out of scope, and so is one written inside a _test.go file.
//
// Two independent checks, because one form of violation does not imply the
// other:
//
//  1. package.Manifest — a SelectorExpr whose X is an Ident named "job" WITH
//     A NIL Obj. The nil-Obj test is what tells the package qualifier
//     "job" (internal/job, imported under its default name) apart from any
//     local variable, parameter or field that happens to be named "job" —
//     go/parser resolves a file-local declaration's Ident to a non-nil *Object
//     of kind Var, but leaves an unresolved package qualifier's Obj nil, since
//     resolving it would need the full import graph rather than one file.
//     Without this, a local `job := ...` unrelated to internal/job would
//     false-positive on any field access whose name contains "Manifest".
//  2. x.Manifest(...) — a CallExpr whose Fun is a SelectorExpr with Sel.Name
//     EXACTLY "Manifest", for ANY receiver x. reconcileResidency (tick.go)
//     already holds a *job.Job named j, not "job", so check 1 alone cannot
//     see `j.Manifest()` reading the manifest's contents — j is a completely
//     different identifier, and the boundary this test polices is "does not
//     read manifest contents through a *job.Job it holds", not "does not
//     spell the package name". This is the actually-likely violation: nothing
//     stops a future reconcileResidency-adjacent function from calling
//     `.Manifest()` on the *job.Job it was already handed to decide residency
//     with, exactly the shape a doc comment used to claim was impossible.
func TestDispatchNamesNoManifestType(t *testing.T) {
	const bannedType = "Manifest"
	const bannedMethod = "Manifest"

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				x, ok := node.X.(*ast.Ident)
				if !ok || x.Name != "job" || x.Obj != nil {
					return true
				}
				if strings.Contains(node.Sel.Name, bannedType) {
					t.Errorf("%s: internal/dispatch names job.%s; the dispatcher "+
						"must delegate WHAT to load to Residency and never read "+
						"manifest contents", name, node.Sel.Name)
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != bannedMethod {
					return true
				}
				t.Errorf("%s: internal/dispatch calls .%s(...); the dispatcher "+
					"must delegate WHAT to load to Residency and never read a "+
					"job's manifest contents, regardless of the receiver's name",
					name, sel.Sel.Name)
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("parsed no files; the walk above would pass vacuously")
	}
}
