package queue

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestAllFetchPolicies_Exhaustive pins AllFetchPolicies against the const
// block it mirrors.
//
// Without this the list is a second copy of the enum with the same defect it
// exists to catch: a policy declared but not listed is invisible to every
// loop over it, so every exhaustiveness test built on the list passes
// vacuously. progress.go is the source of truth; the list only has to agree
// with it. Same construction as postproc.AllQuickCheckOutcomes (#313) and
// constants.AllStatuses (#291).
func TestAllFetchPolicies_Exhaustive(t *testing.T) {
	t.Parallel()

	declared := fetchPolicyConstantsFromSource(t, "progress.go")
	if len(declared) == 0 {
		t.Fatal("parsed no FetchPolicy constants from progress.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[FetchPolicy]bool, len(AllFetchPolicies()))
	for _, p := range AllFetchPolicies() {
		listed[p] = true
	}

	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s is declared in progress.go but missing from AllFetchPolicies(); add it there, then decide what it means at each site that reads the policy — the `!= FetchAlways` aggregates (sizeFigures, recompute, Job.IsComplete, Queue.ForEachUnfinishedArticle), the `== FetchIfNeeded` guards (HasDeferredPar2, DeferredRecoveryIndices, undeferRecovery), and api.fileState's switch", name)
		}
	}
	if len(AllFetchPolicies()) != len(declared) {
		t.Errorf("AllFetchPolicies() has %d entries, progress.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllFetchPolicies()), len(declared))
	}
}

// fetchPolicyConstantsFromSource returns every FetchPolicy constant declared
// in the named file, keyed by constant name and valued by its iota position.
//
// The values are untyped iota rather than string literals, so only the first
// ValueSpec in the block carries the type and the rest inherit it. Counting
// position within the block is what recovers the value; a const block that
// stopped using iota would break that assumption, which the emptiness guard
// above turns into a failure rather than a silent pass.
func fetchPolicyConstantsFromSource(t *testing.T, filename string) map[string]FetchPolicy {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]FetchPolicy)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// Only the block whose first spec is typed FetchPolicy.
		if len(gen.Specs) == 0 {
			continue
		}
		first, ok := gen.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != "FetchPolicy" {
			continue
		}
		for i, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 {
				continue
			}
			found[vs.Names[0].Name] = FetchPolicy(i)
		}
	}
	return found
}
