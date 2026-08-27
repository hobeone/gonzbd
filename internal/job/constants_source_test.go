package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// constantsOfType maps the name of every constant declared with the named type
// in one file to its iota value.
//
// It exists so State and Intent do not each carry their own copy of this AST
// walk. The two enumerations it feeds — TestAllStates_Exhaustive and
// TestAllIntents_Exhaustive — assert the same property about different types,
// and a second copy of the walk is a second thing to keep correct.
//
// Type inheritance follows Go's rule rather than "the last type seen": an
// omitted type inherits only when the spec omits its VALUES too, because such
// a spec repeats the previous one wholesale. A spec that states a value
// without a type begins a fresh untyped run.
// TestStateConstantsFromSource_IotaSemantics' fixture has both shapes.
//
// The value is the constant's index among ALL specs in its const block, not
// among the ones this walk keeps. That distinction is the whole subtlety: iota
// advances for every spec, including a blank `_` and a spec of some other
// type, so counting only the matches shifts every value after the first skip.
// TestStateConstantsFromSource_IotaSemantics pins it against a fixture that
// has exactly that shape — it caught this walk getting it wrong.
func constantsOfType(t *testing.T, filename, typeName string) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]int)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var currentType ast.Expr
		for i, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// A spec's type carries forward only across specs that state
			// NOTHING — the bare continuation lines of an iota run. A spec with
			// expressions and no type starts a fresh, untyped run in Go, so the
			// carried type must be cleared rather than left standing. Without
			// this, `Zeta = 99` after a State run is reported as a State, and
			// reported with the iota-position VALUE, which aliases silently onto
			// whichever real member sits at that index.
			switch {
			case vs.Type != nil:
				currentType = vs.Type
			case len(vs.Values) > 0:
				currentType = nil
			}
			ident, ok := currentType.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == "_" {
					continue
				}
				found[name.Name] = i
			}
		}
	}
	return found
}
