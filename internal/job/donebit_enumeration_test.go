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

// doneBitMarkers is every function in this package that reaches markDone.
//
// Each is a door onto the same bit and each stands on evidence about stable
// storage: ackDurable is the body of AckDurable, which applies a DurableProof
// no path outside a finished barrier can mint; SeedFromRuns and
// ReplaceFromRuns replay the runs such a barrier recorded; MarkArticleDone is
// the bounded per-article door the assembler-facing tier uses.
//
// The list is asserted rather than counted. A count goes green against a call
// site that moved from one function to another, which is exactly the drift the
// prose makes a claim about — jobProgressJSON's doc comment (progress.go)
// names these doors and argues from them that a persisted done bit always
// stands on a completed fsync or a permanent failure.
var doneBitMarkers = []string{
	"MarkArticleDone",
	"ReplaceFromRuns",
	"SeedFromRuns",
	"ackDurable",
}

// doneBitDirectSetters is every function that sets p.done WITHOUT going
// through markDone.
//
// markFailed is here because a permanently failed article is resolved too: its
// bytes will never arrive, so it is Done and Failed both, and the pair is what
// keeps a restart from rewriting a failure as a success (#300).
//
// newJobProgressSized is the one a reader grepping for markDone will not find,
// and the reason this half of the check exists: it writes the bit directly
// because markDone needs a manifest for byte arithmetic that a non-resident
// job has not seeded yet. Its input is the same derived resolution, so the
// claim above survives it.
var doneBitDirectSetters = []string{
	"markDone",
	"markFailed",
	"newJobProgressSized",
}

// TestDoneBitWriters_MatchTheEnumerationStatedInProse holds the enumeration
// that jobProgressJSON's doc comment argues from.
//
// The same enumeration had already gone stale twice when it lived only in
// prose, in two unlinked files, and a grep from either could not reach the
// other. markDone and the done bitset are both unexported, so a parse of this
// one directory sees every writer — that is the property this rests on, and it
// is why the check can be exhaustive without leaving the package.
func TestDoneBitWriters_MatchTheEnumerationStatedInProse(t *testing.T) {
	markers, setters := scanDoneBitWriters(t)

	if !slices.Equal(markers, doneBitMarkers) {
		t.Errorf("functions calling markDone = %v, want %v\n\n"+
			"jobProgressJSON's doc comment in progress.go argues from this list. "+
			"If the new door is correct, add it there and here together, and say "+
			"what evidence it stands on.", markers, doneBitMarkers)
	}
	if !slices.Equal(setters, doneBitDirectSetters) {
		t.Errorf("functions setting p.done directly = %v, want %v\n\n"+
			"A direct writer bypasses markDone's bookkeeping entirely: the byte "+
			"counters, the emitted flag, and the ordering against markFailed.",
			setters, doneBitDirectSetters)
	}
}

// scanDoneBitWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that call markDone and those
// that call x.done.Set.
//
// It reads the directory rather than taking a file list, so a source file
// added later is covered without anyone remembering to name it here.
func scanDoneBitWriters(t *testing.T) (markers, setters []string) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
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
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch {
				case doneBitCalleeName(call.Fun) == "markDone":
					markers = append(markers, fn.Name.Name)
				case setsDoneBit(call.Fun):
					setters = append(setters, fn.Name.Name)
				}
				return true
			})
		}
	}

	// A parse that silently matched nothing would report an empty list, which
	// reads as "the enumeration is now empty" rather than "the scan broke".
	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}

	slices.Sort(markers)
	slices.Sort(setters)
	return slices.Compact(markers), slices.Compact(setters)
}

// doneBitCalleeName returns the identifier a call expression names, for both a
// bare call and a selector call, or "" for anything else.
func doneBitCalleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// setsDoneBit reports whether a call expression is a Set on the done bitset —
// x.done.Set(...) for any receiver x.
//
// Clear is deliberately not matched. Un-marking an article is markNotDone's
// and resetForReload's business; it returns the article to Outstanding rather
// than asserting anything about an fsync, and it is not what the prose claims.
func setsDoneBit(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Set" {
		return false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	return ok && recv.Sel.Name == "done"
}
