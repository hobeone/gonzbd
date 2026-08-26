package job

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ErrFinishRequired's doc comment claims finish is the only mutator that
// assigns Outcome. That is a claim about the whole package's population of
// assignments to the unexported outcome field, not just this file's — and
// Task 8 adds job.go with access to the same unexported field, so the claim
// can be falsified by a file this one never opens.
//
// This makes the enumeration a fact the compiler's own package boundary can
// check, rather than a sentence kept true by memory. It follows the pattern
// in internal/queue/donebit_enumeration_test.go
// (TestDoneBitWriters_MatchTheEnumerationStatedInProse, cited in AGENTS.md),
// but is a different population shape: that test enumerates every FUNCTION
// that reaches one call, this one enumerates every ASSIGNMENT SITE for one
// field. Standing Design Rule 4 rules against duplicating an existing
// enumeration helper for the same population (Task 4's Activity count); this
// is not that — outcome's writer set and Activity's constant count are
// different populations entirely, and neither helper could check the other.
//
// The test asserts the enumeration (the function names), not a bare count. A
// count alone would go green against a write that moved from finish to some
// other function, which is exactly the drift this exists to catch.

// outcomeWriters is every function in this package that sets the unexported
// outcome field, via a plain or compound `=` assignment (`a.outcome = x`,
// `a.outcome += x`), `a.outcome++`/`--`, or a keyed `outcome: x` element in a
// composite literal whose type is Attempt. `:=` declarations and `==`
// comparisons are not assignments to an existing field and are not counted.
// An unkeyed Attempt{...} literal fails the test outright rather than being
// silently miscounted — see the CompositeLit case in scanOutcomeWriters.
// Not covered: a write reached through a second pointer/alias to the same
// Attempt, or reflection.
var outcomeWriters = []string{"finish"}

func TestOutcomeWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanOutcomeWriters(t)
	if !slices.Equal(writers, outcomeWriters) {
		t.Errorf("functions assigning outcome = %v, want %v\n\n"+
			"ErrFinishRequired's doc comment claims finish is the only "+
			"mutator that assigns Outcome. If a second writer is correct "+
			"(e.g. Task 8's Job needs one), say so at that comment AND "+
			"update this list together — a comment that still says "+
			"\"the only mutator\" once a second one exists is worse than no "+
			"comment at all.",
			writers, outcomeWriters)
	}
}

// scanOutcomeWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that set a field named outcome,
// via a plain `=` assignment or via a `outcome: x` composite-literal key.
//
// It reads the directory rather than taking a file list so that a source
// file added later (job.go, in Task 8) is covered without anyone
// remembering to add it here — the failure mode this test exists to close is
// a claim that went stale because nobody opened the file that falsified it.
//
// It inspects each file's whole AST (ast.Inspect(file, ...)), not only the
// bodies of its *ast.FuncDecls: a package-level `var x = Attempt{outcome:
// ...}` is a *ast.GenDecl, which the file.Decls loop used to filter out
// before ever reaching an ast.Inspect call, so such a var bypassed this scan
// entirely. Walking the whole file catches it. A write found outside any
// enclosing function — a package-level var's initializer — is attributed to
// a synthetic "var <name> (package-level)" label rather than a function
// name, so it still shows up as an unexpected entry in the writer set
// instead of being silently dropped for lack of a function to blame.
func scanOutcomeWriters(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	var writers []string
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

		// label tracks what to attribute a write to: the enclosing
		// function's name while walking a FuncDecl's body, or a synthetic
		// package-level label while walking a GenDecl's value expressions
		// (there is no enclosing function for those).
		var label string
		record := func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				// node.Tok == token.DEFINE (`:=`) never applies here: `:=`
				// cannot target an existing selector expression, so every
				// remaining AssignStmt token — ASSIGN and every compound
				// form (+=, |=, ...) — is a write to whatever selector it
				// targets. outcome is a uint8-based enum, so `a.outcome +=
				// x` is legal Go even though nothing in this package does
				// it today.
				if node.Tok == token.DEFINE {
					return true
				}
				for _, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "outcome" {
						writers = append(writers, label)
					}
				}
			case *ast.IncDecStmt:
				// a.outcome++ / a.outcome-- write the field without
				// being an AssignStmt at all.
				if sel, ok := node.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "outcome" {
					writers = append(writers, label)
				}
			case *ast.CompositeLit:
				// &Attempt{outcome: x} sets the field without an
				// AssignStmt at all — the ASSIGN-only scan above cannot
				// see it, which is exactly the gap
				// TestOutcomeWrites_MatchTheEnumerationStatedInProse's
				// doc comment did not disclose. Only a literal whose
				// type is Attempt counts: a bare Ident type check, not
				// "any composite literal with a key or element named
				// outcome" — otherwise an unrelated struct that happens
				// to have its own field called outcome would be
				// misattributed as a write to Attempt.outcome.
				ident, ok := node.Type.(*ast.Ident)
				if !ok || ident.Name != "Attempt" {
					return true
				}
				var unkeyed bool
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						unkeyed = true
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "outcome" {
						writers = append(writers, label)
					}
				}
				// An unkeyed Attempt{...} literal sets fields by
				// position, including outcome, without any key this
				// scan can match against — it would slip past silently
				// rather than being (correctly) counted as a write.
				// Nothing in this package uses one (only newAttempt's
				// keyed Attempt{state: ..., started: ...} exists), so
				// refusing it outright is cheaper and safer than
				// resolving Attempt's field order to a positional
				// index: field order is not otherwise a population this
				// package enumerates or protects, and this scan should
				// not become the thing that breaks if that order
				// changes for an unrelated reason.
				if unkeyed && len(node.Elts) > 0 {
					t.Fatalf("%s: %s constructs an unkeyed Attempt{...} composite literal; "+
						"this scan cannot see which field a positional element sets, so it cannot "+
						"tell whether it writes outcome — use keyed fields (Attempt{state: ...}) instead",
						name, label)
				}
			}
			return true
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				label = d.Name.Name
				ast.Inspect(d.Body, record)
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, val := range vs.Values {
						varName := "?"
						if i < len(vs.Names) {
							varName = vs.Names[i].Name
						}
						label = fmt.Sprintf("var %s (package-level)", varName)
						ast.Inspect(val, record)
					}
				}
			}
		}
	}

	// A parse that silently matched nothing would report an empty list and
	// read as "outcome is never written" rather than "the scan broke".
	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}

	slices.Sort(writers)
	return slices.Compact(writers)
}
