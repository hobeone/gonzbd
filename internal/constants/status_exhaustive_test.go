package constants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// AllStatuses is hand-written, so on its own it is a third copy of the enum
// with the same defect as the two it replaces: a status added to the const
// block but not to the list is invisible to every loop over it, and every
// exhaustiveness test built on it passes vacuously.
//
// This closes that loop by reading the const block itself. status.go is the
// source of truth; AllStatuses only has to agree with it, and this test is
// what makes disagreement a failure rather than a silent gap.
func TestAllStatuses_Exhaustive(t *testing.T) {
	t.Parallel()

	declared := statusConstantsFromSource(t, "status.go")
	if len(declared) == 0 {
		t.Fatal("parsed no Status constants from status.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[Status]bool, len(AllStatuses()))
	for _, s := range AllStatuses() {
		listed[s] = true
	}

	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s (%q) is declared in status.go but missing from AllStatuses(); add it there and give it a phase in queue.Job.Phase()", name, value)
		}
	}
	if len(AllStatuses()) != len(declared) {
		t.Errorf("AllStatuses() has %d entries, status.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllStatuses()), len(declared))
	}
}

// statusConstantsFromSource returns every `Name Status = "value"` constant
// declared in the named file, keyed by constant name.
func statusConstantsFromSource(t *testing.T, filename string) map[string]Status {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]Status)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Only `X Status = "..."`; an untyped or differently-typed
			// const in the same block is not ours.
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Status" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				// lit.Value carries its quotes; strip them rather than
				// pulling in strconv for a known-simple literal.
				found[name.Name] = Status(lit.Value[1 : len(lit.Value)-1])
			}
		}
	}
	return found
}
