package postproc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
)

// TestAllQuickCheckOutcomes_Exhaustive pins AllQuickCheckOutcomes against the
// const block it mirrors.
//
// Without this the list is a second copy of the enum with the same defect it
// exists to catch: an outcome declared but not listed is invisible to every
// loop over it, so every exhaustiveness test built on the list passes
// vacuously. stages.go is the source of truth; the list only has to agree
// with it. Same construction as constants.AllStatuses (#291).
func TestAllQuickCheckOutcomes_Exhaustive(t *testing.T) {
	t.Parallel()

	declared := quickCheckConstantsFromSource(t, "stages.go")
	if len(declared) == 0 {
		t.Fatal("parsed no QuickCheckOutcome constants from stages.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[QuickCheckOutcome]bool, len(AllQuickCheckOutcomes()))
	for _, o := range AllQuickCheckOutcomes() {
		listed[o] = true
	}

	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s is declared in stages.go but missing from AllQuickCheckOutcomes(); add it there and give it an arm in RepairStage.Run's switch", name)
		}
	}
	if len(AllQuickCheckOutcomes()) != len(declared) {
		t.Errorf("AllQuickCheckOutcomes() has %d entries, stages.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllQuickCheckOutcomes()), len(declared))
	}
}

// quickCheckConstantsFromSource returns every QuickCheckOutcome constant
// declared in the named file, keyed by constant name and valued by its iota
// position.
//
// Unlike constants.Status the values are untyped iota rather than string
// literals, so only the first ValueSpec in the block carries the type and the
// rest inherit it. Counting position within the block is what recovers the
// value; a const block that stopped using iota would break that assumption,
// which the emptiness guard above turns into a failure rather than a silent
// pass.
func quickCheckConstantsFromSource(t *testing.T, filename string) map[string]QuickCheckOutcome {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]QuickCheckOutcome)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// Only the block whose first spec is typed QuickCheckOutcome.
		if len(gen.Specs) == 0 {
			continue
		}
		first, ok := gen.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != "QuickCheckOutcome" {
			continue
		}
		for i, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 {
				continue
			}
			found[vs.Names[0].Name] = QuickCheckOutcome(i)
		}
	}
	return found
}

// TestRepairStage_HandlesEveryQuickCheckOutcome walks every declared outcome
// through the stage and asserts each one reaches a decision the stage states
// out loud.
//
// The switch in RepairStage.Run has no compiler-enforced exhaustiveness —
// .golangci.yml does not enable the exhaustive linter — so a new outcome would
// otherwise fall through its default arm and run repair without anyone
// choosing that. Running repair is the fail-safe answer, which is exactly why
// the gap would go unnoticed: the behaviour looks fine and no one decided it.
func TestRepairStage_HandlesEveryQuickCheckOutcome(t *testing.T) {
	t.Parallel()

	for _, outcome := range AllQuickCheckOutcomes() {
		t.Run(outcome.String(), func(t *testing.T) {
			t.Parallel()

			job, dir := stageJob(t)
			copyPar2Fixtures(t, dir)
			// DirectUnpack clean, so every outcome faces the same decision:
			// whether its own verdict permits the shortcut.
			job.DirectUnpackSets = map[string]directunpack.SuccessSet{
				"ok": {RarParts: []string{"data.bin"}, ExtractedFiles: []string{"data.out"}},
			}
			job.QuickCheck = outcome

			stage := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
			if err := stage.Run(t.Context(), job); err != nil {
				t.Fatalf("Run: %v", err)
			}

			for _, line := range job.OutputLines {
				if line == "[repair] Running: unhandled QuickCheck outcome "+outcome.String() {
					t.Fatalf("%s reached the switch's default arm; add an explicit case for it in RepairStage.Run", outcome)
				}
			}
		})
	}
}

// The default arm has to be reachable and has to say so, or the guard above
// is asserting against a branch that cannot fire.
func TestRepairStage_UnhandledQuickCheckOutcomeSaysSo(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)
	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"ok": {RarParts: []string{"data.bin"}, ExtractedFiles: []string{"data.out"}},
	}
	// A value no case names, standing in for one added to the enum later.
	job.QuickCheck = QuickCheckOutcome(99)

	stage := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	warned := false
	for _, line := range job.OutputLines {
		if line == "[repair] Running: unhandled QuickCheck outcome unknown" {
			warned = true
		}
		if line == "[repair] Skipped: Direct Unpack successfully extracted all archives during download" {
			t.Error("an unrecognised outcome took the DirectUnpack shortcut; an unknown verdict must never authorise skipping verification")
		}
	}
	if !warned {
		t.Errorf("an unrecognised outcome passed through silently; output: %v", job.OutputLines)
	}
}
