package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestState_String(t *testing.T) {
	for _, tc := range []struct {
		s    State
		want string
	}{
		{Waiting, "Waiting"},
		{Fetching, "Fetching"},
		{Assessing, "Assessing"},
		{Repairing, "Repairing"},
		{Extracting, "Extracting"},
		{Finalizing, "Finalizing"},
		{Finished, "Finished"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestState_StringUnknown(t *testing.T) {
	if got := State(200).String(); got != "State(200)" {
		t.Errorf("String() = %q, want %q", got, "State(200)")
	}
}

// TestAllStates_Exhaustive parses state.go and fails if a declared State
// constant is missing from AllStates(). state.go is the single source of
// truth; AllStates only has to agree with it. Mirrors the same guard
// internal/constants/status_exhaustive_test.go applies to Status — a
// hand-written list is otherwise a second copy of the enum that goes stale
// silently.
func TestAllStates_Exhaustive(t *testing.T) {
	declared := stateConstantsFromSource(t, "state.go")
	if len(declared) == 0 {
		t.Fatal("parsed no State constants from state.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[State]bool, len(AllStates()))
	for _, s := range AllStates() {
		listed[s] = true
	}
	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s is declared in state.go but missing from AllStates(); add it there and give it edges in transition.go", name)
		}
	}
	if len(AllStates()) != len(declared) {
		t.Errorf("AllStates() has %d entries, state.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllStates()), len(declared))
	}
}

// stateConstantsFromSource returns every `Name State = iota`-style constant
// declared in filename, keyed by name, with its resolved value.
func stateConstantsFromSource(t *testing.T, filename string) map[string]State {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]State)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var idx State
		var isStateBlock bool
		for i, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if i == 0 {
				ident, ok := vs.Type.(*ast.Ident)
				isStateBlock = ok && ident.Name == "State"
			}
			if !isStateBlock {
				break
			}
			for _, name := range vs.Names {
				if name.Name == "_" {
					idx++
					continue
				}
				found[name.Name] = idx
				idx++
			}
		}
	}
	return found
}
