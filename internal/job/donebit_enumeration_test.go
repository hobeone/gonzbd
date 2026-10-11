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

// doneMarkers is every function in this package that reaches markDone.
var doneMarkers = []string{
	"MarkArticleWritten",
	"installRows",
}

// bitsetWriters is, for each write to one of JobProgress's three bitsets,
// every function in this package's non-test sources that makes it. The prose
// on JobProgress's bitsets and on clearDone states these sets.
var bitsetWriters = map[string][]string{
	"done.Set":   {"markDone", "setFailedBits"},
	"done.Clear": {"clearDone"},

	"failed.Set":   {"setFailedBits"},
	"failed.Clear": {"clearDone"},

	"emitted.Set": {"markEmitted"},
	// ClearArticleEmitted clears the bit alone only while the manifest is
	// evicted, leaving the counters to RestoreContent's recompute.
	"emitted.Clear": {"ClearArticleEmitted", "clearEmitted", "markDone", "resetForReload", "setFailedBits"},
}

func TestDoneBitWriters_MatchTheEnumerationStatedInProse(t *testing.T) {
	markers, writes := scanBitsetWriters(t)

	if !slices.Equal(markers, doneMarkers) {
		t.Errorf("functions calling markDone = %v, want %v", markers, doneMarkers)
	}
	if got, want := writes["done.Set"], bitsetWriters["done.Set"]; !slices.Equal(got, want) {
		t.Errorf("functions setting p.done directly = %v, want %v", got, want)
	}
}

// TestBitsetWriters_MatchTheEnumerationStatedInProse pins every direct
// Set/Clear on the done, failed and emitted bitsets to the functions named in
// bitsetWriters, so a new writer fails here by name.
func TestBitsetWriters_MatchTheEnumerationStatedInProse(t *testing.T) {
	_, writes := scanBitsetWriters(t)

	for op, want := range bitsetWriters {
		if got := writes[op]; !slices.Equal(got, want) {
			t.Errorf("functions calling %s = %v, want %v", op, got, want)
		}
	}
	for op, got := range writes {
		if _, ok := bitsetWriters[op]; !ok {
			t.Errorf("functions calling %s = %v, and bitsetWriters does not list the operation", op, got)
		}
	}
}

// scanBitsetWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that call markDone, and, keyed
// by "field.Method", of those that call Set or Clear on the done, failed or
// emitted bitset.
func scanBitsetWriters(t *testing.T) (markers []string, writes map[string][]string) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	writes = make(map[string][]string)
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
				if calleeName(call.Fun) == "markDone" {
					markers = append(markers, fn.Name.Name)
				}
				if op, ok := bitsetWrite(call.Fun); ok {
					writes[op] = append(writes[op], fn.Name.Name)
				}
				return true
			})
		}
	}

	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}

	slices.Sort(markers)
	for op, fns := range writes {
		slices.Sort(fns)
		writes[op] = slices.Compact(fns)
	}
	return slices.Compact(markers), writes
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

// bitsetWrite reports whether a call expression is x.<field>.Set(...) or
// x.<field>.Clear(...) on one of the three bitsets, for any receiver x, and
// names it "field.Method".
func bitsetWrite(fun ast.Expr) (string, bool) {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Set" && sel.Sel.Name != "Clear") {
		return "", false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	switch recv.Sel.Name {
	case "done", "failed", "emitted":
		return recv.Sel.Name + "." + sel.Sel.Name, true
	}
	return "", false
}
