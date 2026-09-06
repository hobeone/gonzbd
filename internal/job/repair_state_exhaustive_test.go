package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestAllRepairStates_Exhaustive pins AllRepairStates against the const block
// it mirrors.
//
// Without this the list is a second copy of the enum carrying the same defect
// it exists to catch: a state declared but not listed is invisible to every
// loop over it, so every exhaustiveness test built on the list passes
// vacuously. repair.go is the source of truth; the list only has to agree with
// it. Same construction as AllFetchPolicies (#331),
// postproc.AllQuickCheckOutcomes (#313) and constants.AllStatuses (#291).
func TestAllRepairStates_Exhaustive(t *testing.T) {
	t.Parallel()

	declared := repairStateConstantsFromSource(t, "repair.go")
	if len(declared) == 0 {
		t.Fatal("parsed no RepairState constants from repair.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[RepairState]bool, len(AllRepairStates()))
	for _, s := range AllRepairStates() {
		listed[s] = true
	}

	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s is declared in repair.go but missing from AllRepairStates(); add it there, "+
				"then decide what it means at each site that reads the verdict — Hopeless() (which the "+
				"dispatcher's Early Health Gate acts on), failMsgForJob's wording switch, and "+
				"HEALTH_LABELS in ui/src/lib/components/QueueRow.svelte", name)
		}
	}
	if len(AllRepairStates()) != len(declared) {
		t.Errorf("AllRepairStates() has %d entries, repair.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllRepairStates()), len(declared))
	}
}

// TestRepairState_WireValues pins the strings themselves, not just the set.
//
// These values cross the API boundary into a TypeScript union in
// ui/src/lib/types.ts. A rename here compiles, passes every Go test that
// compares states to constants, and satisfies contract_test.go — which asserts
// that the repair_state key exists, not what it may contain. The only visible
// symptom would be the queue row quietly falling back to a neutral "Unknown".
func TestRepairState_WireValues(t *testing.T) {
	t.Parallel()

	// Mirrors the RepairState union in ui/src/lib/types.ts.
	want := map[RepairState]bool{
		"intact":          true,
		"repairable":      true,
		"unknown":         true,
		"no_capacity":     true,
		"beyond_capacity": true,
	}

	for _, s := range AllRepairStates() {
		if !want[s] {
			t.Errorf("RepairState %q is not in the union declared by ui/src/lib/types.ts; "+
				"the queue row would render it as a neutral fallback rather than a verdict. "+
				"Update the union and HEALTH_LABELS in QueueRow.svelte together with this constant.", s)
		}
		delete(want, s)
	}
	for s := range want {
		t.Errorf("ui/src/lib/types.ts declares %q but no RepairState constant produces it; "+
			"the union has an entry the backend can never send", s)
	}
}

// repairStateConstantsFromSource returns every RepairState constant declared
// in the named file, keyed by constant name and valued by its string literal.
//
// Unlike the FetchPolicy equivalent these are explicit string literals rather
// than iota, so the value comes from the ValueSpec directly and position in
// the block is irrelevant. A const block that stopped using string literals
// would yield nothing, which the emptiness guard above turns into a failure
// rather than a silent pass.
func repairStateConstantsFromSource(t *testing.T, filename string) map[string]RepairState {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]RepairState)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "RepairState" {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", vs.Names[0].Name, err)
			}
			found[vs.Names[0].Name] = RepairState(value)
		}
	}
	return found
}
