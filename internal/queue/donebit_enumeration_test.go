package queue

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

// The done bit's writers are enumerated in prose at three sites, and the
// enumeration has been found short twice — once at queue/progress.go and
// again, months later, at the sibling copy in app/statusinfo.go that the
// first sweep never opened. Nothing failed either time. A reviewer caught
// both.
//
// This file makes the enumeration a fact the compiler's own package boundary
// can enforce, rather than three paragraphs that must be kept in step by
// memory. markDone is UNEXPORTED, so every call to it is inside this package
// and a parse of this directory sees all of them — that is the property the
// test rests on, and it is why the check can live here and still be
// exhaustive.
//
// The three prose sites, all of which must be updated together when this test
// legitimately fails:
//
//   - internal/queue/progress.go, jobProgressJSON's doc comment
//   - internal/app/statusinfo.go, JobDurability's doc comment
//   - internal/app/statusinfo_test.go, TestJobDurability_ReportsDownloadedBytes-
//     AsDurable, which restates the enumeration at the assertion
//
// The third path said internal/app/durability_test.go until B2.4a, and no such
// file exists — the enumeration's own map to its copies had drifted. It is
// corrected above rather than noted, because a wrong path here costs the next
// sweep the site it was written to protect. The third copy states the
// enumeration by ROLE ("the ack, and the replays of the runs a barrier
// recorded") rather than by function name, so unlike the other two it does not
// go stale when a name moves.
//
// The test asserts the enumeration, not a count. A bare count would go green
// against a call site that moved from one function to another, which is
// exactly the drift the prose is making a claim about.

// doneMarkers is every function in this package that reaches markDone.
//
// Each is a door onto the same bit, and each stands on a completed fsync:
// ackDurable applies a DurableProof no path outside a finished barrier can
// mint; seedFromRuns and ReplaceFromRuns replay the runs such a barrier
// recorded; applyResolution replays the resolution derived from those same
// records when a job is re-hydrated.
//
// Two of the four are unexported *Job methods since B2.4a, where they were
// Queue method bodies. The door did not move — Queue.AckDurable and
// Queue.SeedFromRuns are still the only callers, and Queue.AckDurable still
// takes the DurableProof and unwraps it before calling ackDurable, so the
// evidence each door stands on is unchanged. What moved is which function
// name this scan reports, which is precisely the drift the prose sites must
// be kept in step with.
var doneMarkers = []string{
	"ReplaceFromRuns",
	"ackDurable",
	"applyResolution",
	"seedFromRuns",
}

// doneBitSetters is every function that sets p.done WITHOUT going through
// markDone.
//
// markFailed is here because a permanently failed article is resolved too —
// its bytes will never arrive, so it is Done and Failed both, and the pair is
// what keeps #300's restart from rewriting a failure as a success.
//
// newJobProgressSized is the one that a reader grepping for markDone will not
// find, and the reason this half of the check exists at all: it writes the bit
// directly because markDone needs a manifest for byte arithmetic that a
// non-resident job has not seeded yet. Its input is the same derived
// resolution, so the identity survives it.
var doneBitSetters = []string{
	"markDone",
	"markFailed",
	"newJobProgressSized",
}

func TestDoneBitWriters_MatchTheEnumerationStatedInProse(t *testing.T) {
	markers, setters := scanDoneBitWriters(t)

	if !slices.Equal(markers, doneMarkers) {
		t.Errorf("functions calling markDone = %v, want %v\n\n"+
			"The done bit's doors are enumerated in prose at three sites; this "+
			"list moving means at least one of them now says something false. "+
			"Update all three together — see this file's doc comment for the "+
			"paths, and note that two of them are in internal/app, which no "+
			"grep of internal/queue will reach.",
			markers, doneMarkers)
	}

	if !slices.Equal(setters, doneBitSetters) {
		t.Errorf("functions setting p.done directly = %v, want %v\n\n"+
			"A new direct writer bypasses markDone's bookkeeping entirely: the "+
			"byte counters, the emitted flag, and the first-writer-wins "+
			"ordering against markFailed. If the new writer is correct, say so "+
			"at all three prose sites AND here — statusinfo.go names its direct "+
			"writer explicitly rather than hiding it behind the word "+
			"\"ultimately\", and that sentence is the one this guards.",
			setters, doneBitSetters)
	}
}

// scanDoneBitWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that call markDone and those
// that call p.done.Set.
//
// It reads the directory rather than taking a file list so that a source file
// added later is covered without anyone remembering to add it here — the
// failure mode this whole test exists to close is a claim that went stale
// because nobody opened the file that falsified it.
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
				case calleeName(call.Fun) == "markDone":
					markers = append(markers, fn.Name.Name)
				case setsDoneBit(call.Fun):
					setters = append(setters, fn.Name.Name)
				}
				return true
			})
		}
	}

	// A parse that silently matched nothing would report an empty list and
	// read as "the enumeration is now empty" rather than "the scan broke".
	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}

	slices.Sort(markers)
	slices.Sort(setters)
	return slices.Compact(markers), slices.Compact(setters)
}

// calleeName returns the identifier a call expression names, for both a bare
// call and a selector call, or "" for anything else.
func calleeName(fun ast.Expr) string {
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
// and resetForReload's business, it returns the article to Outstanding rather
// than asserting anything about an fsync, and it is not what the prose
// enumerations claim.
func setsDoneBit(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Set" {
		return false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	return ok && recv.Sel.Name == "done"
}
