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
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse file %s: %v", entry.Name(), err)
		}
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
					"manifest contents", entry.Name(), sel.Sel.Name)
			}
			return true
		})
	}
}
