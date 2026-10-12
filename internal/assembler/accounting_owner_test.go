package assembler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// accountingReaders is, for each of fileWriter's three accounting fields,
// every function in this package's non-test sources that names it. The field
// comments on seenDone, seenFailed and partsWritten state these sets; this is
// what makes them a test rather than a recollection.
//
// Go cannot make "an exit path that applies no disposition" a compile error,
// so this is the enforced form: a function outside this list that reaches
// into the maps or the counter — a second owner — fails here by name.
var accountingReaders = map[string][]string{
	"seenDone":     {"admit", "holdsPart", "resolve"},
	"seenFailed":   {"holdsPart", "resolve"},
	"partsWritten": {"admit", "parts", "resolve"},
}

// resolveCallers is every function in this package's non-test sources that
// calls resolve, as resolve's own comment states it.
var resolveCallers = []string{"handleFatalArticle", "handleSuccessArticle"}

// TestFileWriterAccounting_HasOneOwner pins that the writer's accounting is
// read and moved only by the functions accountingReaders names, and that
// resolve is reached only from the two callers its comment enumerates.
func TestFileWriterAccounting_HasOneOwner(t *testing.T) {
	readers, callers := scanAccountingUses(t)

	for field, want := range accountingReaders {
		if got := readers[field]; !slices.Equal(got, want) {
			t.Errorf("functions naming %s = %v, want %v", field, got, want)
		}
	}
	for field, got := range readers {
		if _, ok := accountingReaders[field]; !ok {
			t.Errorf("functions naming %s = %v, and accountingReaders does not list the field", field, got)
		}
	}
	if !slices.Equal(callers, resolveCallers) {
		t.Errorf("functions calling resolve = %v, want %v", callers, resolveCallers)
	}
}

// scanAccountingUses parses this package's non-test sources and returns, keyed
// by field, the sorted, deduplicated names of the functions whose bodies
// select seenDone, seenFailed or partsWritten on any receiver, and the names
// of the functions that call resolve.
func scanAccountingUses(t *testing.T) (readers map[string][]string, callers []string) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	readers = make(map[string][]string)
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
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if _, tracked := accountingReaders[x.Sel.Name]; tracked {
						readers[x.Sel.Name] = append(readers[x.Sel.Name], fn.Name.Name)
					}
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "resolve" {
						callers = append(callers, fn.Name.Name)
					}
				}
				return true
			})
		}
	}

	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}

	for _, field := range slices.Collect(maps.Keys(readers)) {
		slices.Sort(readers[field])
		readers[field] = slices.Compact(readers[field])
	}
	slices.Sort(callers)
	return readers, slices.Compact(callers)
}
