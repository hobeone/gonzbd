package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
//
// The iota value is the ValueSpec's own position within the const block
// (Go's real iota semantics: it advances once per spec, not once per
// identifier), so a spec naming several identifiers
// (`A, B State = iota, iota`) gives them the same value, matching what the
// compiler would give them — the earlier version of this scan advanced its
// counter per identifier instead and would have disagreed.
//
// A spec's type is only reconsidered when the spec states one explicitly;
// an omitted type (every spec after the first in a plain iota block)
// inherits the last explicit type instead of ending the scan. That is what
// lets an untyped leading spec, or a `_ = iota` skip line with no type of
// its own, sit ahead of the typed State constants without truncating the
// block the earlier version did, by deciding "is this a State block?" only
// once, from gd.Specs[0].
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
		var currentType ast.Expr
		for i, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if vs.Type != nil {
				currentType = vs.Type
			}
			ident, ok := currentType.(*ast.Ident)
			if !ok || ident.Name != "State" {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == "_" {
					continue
				}
				found[name.Name] = State(i)
			}
		}
	}
	return found
}

// TestStateConstantsFromSource_IotaSemantics pins stateConstantsFromSource
// against const-block shapes that never appear in state.go today (one
// constant per line, all typed on the first spec) but that Go's iota rules
// still have to be interpreted correctly for: a leading spec that carries no
// type at all (`_ = iota`, not `_ State = iota` — the untyped case is the
// one the old scan's `gd.Specs[0]`-only check could not see past), and a
// spec naming more than one identifier. It uses a fixture file rather than
// mutating state.go itself, because stateConstantsFromSource already takes
// filename as a parameter for exactly this kind of test — a temp fixture
// exercises the parser the same way a real file would, without risking a
// real State constant briefly disagreeing with AllStates() mid-test.
func TestStateConstantsFromSource_IotaSemantics(t *testing.T) {
	const src = `package job

const (
	_ = iota // untyped: the old scan decided isStateBlock here and broke the whole scan
	Alpha State = iota
	Beta
	Gamma, Delta State = iota, iota // one spec, two names, same iota
	Epsilon
)
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got := stateConstantsFromSource(t, path)
	want := map[string]State{
		"Alpha":   1,
		"Beta":    2,
		"Gamma":   3,
		"Delta":   3,
		"Epsilon": 4,
	}
	if len(got) != len(want) {
		t.Fatalf("stateConstantsFromSource(fixture) = %v, want %v", got, want)
	}
	for name, wantVal := range want {
		if gotVal, ok := got[name]; !ok || gotVal != wantVal {
			t.Errorf("stateConstantsFromSource(fixture)[%q] = %v (ok=%v), want %v", name, gotVal, ok, wantVal)
		}
	}
}
