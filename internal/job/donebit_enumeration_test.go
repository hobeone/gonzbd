package job

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

// doneMarkers is every function in this package that reaches markDone.
var doneMarkers = []string{
	"AckDurable",
	"ApplyResolution",
	"MarkArticleDone",
	"ReplaceFromRuns",
	"SeedFromRuns",
}

// doneBitSetters is every function that sets p.done WITHOUT going through
// markDone.
var doneBitSetters = []string{
	"markDone",
	"markFailed",
	"newJobProgressSized",
}

func TestDoneBitWriters_MatchTheEnumerationStatedInProse(t *testing.T) {
	markers, setters := scanDoneBitWriters(t)

	if !slices.Equal(markers, doneMarkers) {
		t.Errorf("functions calling markDone = %v, want %v", markers, doneMarkers)
	}

	if !slices.Equal(setters, doneBitSetters) {
		t.Errorf("functions setting p.done directly = %v, want %v", setters, doneBitSetters)
	}
}

// scanDoneBitWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that call markDone and those
// that call p.done.Set.
func scanDoneBitWriters(t *testing.T) (markers, setters []string) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
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
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch {
				case calleeName(call.Fun) == "markDone":
					markers = append(markers, fn.Name.Name)
				case setsDoneBit(call.Fun):
					setters = append(setters, fn.Name.Name)
				}
				return true
			})
		}
	}

	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}

	slices.Sort(markers)
	slices.Sort(setters)
	return slices.Compact(markers), slices.Compact(setters)
}

// calleeName returns the identifier a call expression names, for both a bare
// call and a selector call, or "" for anything else.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// setsDoneBit reports whether a call expression is a Set on the done bitset —
// x.done.Set(...) for any receiver x.
func setsDoneBit(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Set" {
		return false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	return ok && recv.Sel.Name == "done"
}
