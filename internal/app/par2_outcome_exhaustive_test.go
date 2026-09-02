package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestAllPar2Outcomes_Exhaustive pins allPar2Outcomes against the const block
// it mirrors.
//
// Without this the list is a second copy of the enum with the same defect it
// exists to catch: an outcome declared but not listed is invisible to every
// loop over it, so every exhaustiveness test built on the list passes
// vacuously. app.go is the source of truth; the list only has to agree with
// it. Same construction as postproc.AllQuickCheckOutcomes
// (internal/postproc/quickcheck_exhaustive_test.go).
func TestAllPar2Outcomes_Exhaustive(t *testing.T) {
	t.Parallel()

	declared := par2OutcomeConstantsFromSource(t, "app.go")
	if len(declared) == 0 {
		t.Fatal("parsed no par2Outcome constants from app.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[par2Outcome]bool, len(allPar2Outcomes()))
	for _, o := range allPar2Outcomes() {
		listed[o] = true
	}

	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s is declared in app.go but missing from allPar2Outcomes(); add it there and give it an arm in maybeReleaseRecoveryVolumes's switch", name)
		}
	}
	if len(allPar2Outcomes()) != len(declared) {
		t.Errorf("allPar2Outcomes() has %d entries, app.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(allPar2Outcomes()), len(declared))
	}
}

// par2OutcomeConstantsFromSource returns every par2Outcome constant declared
// in the named file, keyed by constant name and valued by its iota position.
//
// The values are untyped iota rather than string literals, so only the first
// ValueSpec in the block carries the type and the rest inherit it. Counting
// position within the block is what recovers the value; a const block that
// stopped using iota would break that assumption, which the emptiness guard
// above turns into a failure rather than a silent pass.
func par2OutcomeConstantsFromSource(t *testing.T, filename string) map[string]par2Outcome {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]par2Outcome)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// Only the block whose first spec is typed par2Outcome.
		if len(gen.Specs) == 0 {
			continue
		}
		first, ok := gen.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != "par2Outcome" {
			continue
		}
		for i, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 {
				continue
			}
			found[vs.Names[0].Name] = par2Outcome(i)
		}
	}
	return found
}
