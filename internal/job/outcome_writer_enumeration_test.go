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
// outcome field, either via `Tok == token.ASSIGN` or via a `outcome: x` key
// in a composite literal. `:=` declarations and `==` comparisons are not
// assignments to an existing field and are not counted.
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
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					if node.Tok != token.ASSIGN {
						return true
					}
					for _, lhs := range node.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "outcome" {
							writers = append(writers, fn.Name.Name)
						}
					}
				case *ast.CompositeLit:
					// &Attempt{outcome: x} sets the field without an
					// AssignStmt at all — the ASSIGN-only scan above cannot
					// see it, which is exactly the gap
					// TestOutcomeWrites_MatchTheEnumerationStatedInProse's
					// doc comment did not disclose.
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == "outcome" {
							writers = append(writers, fn.Name.Name)
						}
					}
				}
				return true
			})
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
