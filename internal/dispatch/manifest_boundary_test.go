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
// 1). The dispatcher decides WHEN a job is resident and delegates WHAT to load;
// naming the manifest type here is how that separation gets lost.
//
// It parses this package's own source rather than grepping, so a reference in a
// comment or a string does not trip it and a reference in a type does.
func TestDispatchNamesNoManifestType(t *testing.T) {
	const banned = "Manifest"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
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
