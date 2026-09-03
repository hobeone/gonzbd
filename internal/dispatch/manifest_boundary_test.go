package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestDispatchNamesNoManifestType is the residency boundary, demoted from a
// compiler guarantee to a test one when Manifest moved into internal/job (Task
// 1). Before the move, dispatch could not name the manifest type at all: it
// lived in internal/queue, which this package may not import. It now lives in
// internal/job, which this package DOES import, so nothing but this test
// stands between the dispatcher and the manifest's contents.
//
// The dispatcher decides WHEN a job is resident and delegates WHAT to load to
// Residency; naming the manifest type here is how that separation gets lost.
//
// It parses this package's own non-test source rather than grepping, so a
// reference in a comment or a string does not trip it and a reference in a
// type does. It sees only THIS package: a violation in internal/dispatch/store
// is out of scope, and so is one written inside a _test.go file.
func TestDispatchNamesNoManifestType(t *testing.T) {
	const banned = "Manifest"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no packages; the walk below would pass vacuously")
	}

	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok || x.Name != "job" {
					return true
				}
				if strings.Contains(sel.Sel.Name, banned) {
					t.Errorf("%s: internal/dispatch names job.%s; the dispatcher "+
						"must delegate WHAT to load to Residency and never read "+
						"manifest contents", name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}
