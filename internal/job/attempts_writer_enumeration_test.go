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

// BeginAttempt's boundary guard (job.go) checks only the most recent
// element of j.attempts, on the premise that BeginAttempt is the only place
// that ever appends (or otherwise assigns) to that field. A grep result
// frozen in a comment records that someone once checked; it does not keep
// enforcing the claim as the package changes. This test does — it parses
// the package's non-test sources itself and asserts the writer set is
// exactly {BeginAttempt}, the same pattern
// TestOutcomeWrites_MatchTheEnumerationStatedInProse uses for Attempt.outcome
// in outcome_writer_enumeration_test.go, applied to a different population
// (every write site for Job.attempts, not Attempt.outcome).
var attemptsWriters = []string{"BeginAttempt"}

func TestAttemptsWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanAttemptsWriters(t)
	if !slices.Equal(writers, attemptsWriters) {
		t.Errorf("functions writing j.attempts = %v, want %v\n\n"+
			"BeginAttempt's doc comment claims it is the only place "+
			"j.attempts is appended to, which is what lets the boundary "+
			"guard check just the last element. If a second writer is "+
			"correct, say so at that comment AND update this list "+
			"together — a comment that still says \"the only place\" once "+
			"a second writer exists is worse than no comment at all.",
			writers, attemptsWriters)
	}
}

// scanAttemptsWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that write to a field named
// attempts, via a plain `=` assignment to `j.attempts` (which covers
// `j.attempts = append(j.attempts, ...)`, the shape BeginAttempt uses) or to
// `j.attempts[i]` (an IndexExpr on the same selector), or via an
// `attempts: x` composite-literal key on a literal whose type is Job.
//
// It reads the directory rather than taking a file list so that a source
// file added later is covered without anyone remembering to add it here —
// the failure mode this test exists to close is a claim that went stale
// because nobody opened the file that falsified it.
func scanAttemptsWriters(t *testing.T) []string {
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
						// A plain `j.attempts = ...` is a SelectorExpr on the
						// left. `j.attempts[i] = ...` is an IndexExpr whose
						// .X is that same SelectorExpr — the SelectorExpr
						// case alone never matches an IndexExpr node, so a
						// single-element write via indexing was invisible to
						// this scan before this case was added.
						sel, ok := lhs.(*ast.SelectorExpr)
						if idx, isIndex := lhs.(*ast.IndexExpr); isIndex {
							sel, ok = idx.X.(*ast.SelectorExpr)
						}
						if ok && sel.Sel.Name == "attempts" {
							writers = append(writers, fn.Name.Name)
						}
					}
				case *ast.CompositeLit:
					// Only a literal whose type is Job counts — matching any
					// "attempts: x" key regardless of the literal's type
					// would misattribute an unrelated struct that happens to
					// have its own field called attempts, the same gap
					// scanOutcomeWriters' CompositeLit case closes for
					// Attempt.outcome (outcome_writer_enumeration_test.go).
					ident, ok := node.Type.(*ast.Ident)
					if !ok || ident.Name != "Job" {
						return true
					}
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "attempts" {
							writers = append(writers, fn.Name.Name)
						}
					}
				}
				return true
			})
		}
	}

	// A parse that silently matched nothing would report an empty list and
	// read as "attempts is never written" rather than "the scan broke".
	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}
	if len(writers) == 0 {
		t.Fatal("scan found zero writers of j.attempts; BeginAttempt does " +
			"write it, so an empty result means the scan itself is broken, " +
			"not that the population is empty")
	}

	slices.Sort(writers)
	return slices.Compact(writers)
}
