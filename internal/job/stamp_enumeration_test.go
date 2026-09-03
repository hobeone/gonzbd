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

// stampFieldOwner names the struct declaring each field these tests scan for.
// scanStampWriters uses it to tell a keyed `field: x` composite-literal element
// that legitimately sets the field apart from an unrelated struct reusing the
// name. TestStampFieldOwners_AreTheOnlyDeclarers enforces the mapping; this is
// the table it enforces, not the guarantee itself.
var stampFieldOwner = map[string]string{
	"downloadStarted":  "JobProgress",
	"downloadFinished": "JobProgress",
}

// downloadStartedWriters and downloadFinishedWriters are the enumerations these
// tests defend: the functions permitted to assign p.downloadStarted or
// p.downloadFinished by name.
var downloadStartedWriters = []string{
	"clearDownloadStamps", "restoreDownloadStamps", "setDownloadStartedOnce",
}

var downloadFinishedWriters = []string{
	"clearDownloadStamps", "restoreDownloadStamps", "setDownloadFinishedOnce",
}

// scanStampWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that write the unexported field
// named field.
func scanStampWriters(t *testing.T, field string) []string {
	t.Helper()

	owner := stampFieldOwner[field]

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

		var label string
		record := func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				if node.Tok == token.DEFINE {
					return true
				}
				for _, lhs := range node.Lhs {
					lhs = ast.Unparen(lhs)
					sel, ok := lhs.(*ast.SelectorExpr)
					if idx, isIndex := lhs.(*ast.IndexExpr); isIndex {
						sel, ok = ast.Unparen(idx.X).(*ast.SelectorExpr)
					}
					if ok && sel.Sel.Name == field {
						writers = append(writers, label)
					}
				}
			case *ast.IncDecStmt:
				if sel, ok := ast.Unparen(node.X).(*ast.SelectorExpr); ok && sel.Sel.Name == field {
					writers = append(writers, label)
				}
			case *ast.UnaryExpr:
				if node.Op != token.AND {
					return true
				}
				if sel, ok := ast.Unparen(node.X).(*ast.SelectorExpr); ok && sel.Sel.Name == field {
					writers = append(writers, label)
				}
			case *ast.CompositeLit:
				if node.Type == nil {
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
							t.Fatalf("%s: %s builds a composite literal with an elided type that "+
								"sets %s; this scan cannot tell which type it constructs, so it "+
								"can neither attribute nor dismiss the write — name the element "+
								"type explicitly at that literal", name, label, field)
						}
					}
					return true
				}
				ident, ok := node.Type.(*ast.Ident)
				if !ok || ident.Name != owner {
					return true
				}
				var unkeyed bool
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						unkeyed = true
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
						writers = append(writers, label)
					}
				}
				if unkeyed && len(node.Elts) > 0 {
					t.Fatalf("%s: %s constructs an unkeyed %s{...} composite literal; "+
						"this scan cannot see which field a positional element sets, so it "+
						"cannot tell whether it writes %s — use keyed fields instead",
						name, label, owner, field)
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

	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}
	if len(writers) == 0 {
		t.Fatalf("scan found zero writers of %s; an empty result reads identically to "+
			"\"nobody writes it\" and means the scan is broken, not that the "+
			"population is empty", field)
	}

	slices.Sort(writers)
	return slices.Compact(writers)
}

// TestDownloadStampWriters_MatchTheEnumerationStatedInProse asserts that the
// downloadStarted and downloadFinished timestamps are only written by the
// designated owner methods.
func TestDownloadStampWriters_MatchTheEnumerationStatedInProse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		field string
		want  []string
	}{
		{"downloadStarted", downloadStartedWriters},
		{"downloadFinished", downloadFinishedWriters},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			writers := scanStampWriters(t, tc.field)
			if !slices.Equal(writers, tc.want) {
				t.Errorf("functions assigning %s = %v, want %v\n\n"+
					"setDownloadStartedOnce's doc comment (progress.go) claims the owner "+
					"methods are the only functions in this package's non-test sources that "+
					"assign either stamp by name.",
					tc.field, writers, tc.want)
			}
		})
	}
}

// TestStampFieldOwners_AreTheOnlyDeclarers enforces that only JobProgress
// declares downloadStarted and downloadFinished.
func TestStampFieldOwners_AreTheOnlyDeclarers(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	declarers := make(map[string][]string)
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
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if ts.Assign != token.NoPos {
				if id, isIdent := ts.Type.(*ast.Ident); isIdent {
					for _, owner := range stampFieldOwner {
						if id.Name == owner {
							t.Errorf("%s declares `type %s = %s`; scanStampWriters matches a "+
								"composite literal by its type NAME, so %s{...} setting a "+
								"tracked field would not be counted and the writer enumeration "+
								"would pass with a writer it never saw. Use the type directly, "+
								"or teach the scan to resolve aliases.", name, ts.Name.Name, owner, ts.Name.Name)
							break
						}
					}
				}
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, f := range st.Fields.List {
				for _, fn := range f.Names {
					if _, tracked := stampFieldOwner[fn.Name]; tracked {
						declarers[fn.Name] = append(declarers[fn.Name], ts.Name.Name)
					}
				}
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("scanned no non-test files; this test would pass vacuously")
	}

	for field, owner := range stampFieldOwner {
		got := declarers[field]
		if len(got) == 0 {
			t.Errorf("no struct in this package declares %q, but stampFieldOwner maps it to %s; "+
				"the field was renamed or removed and stampFieldOwner was not updated", field, owner)
			continue
		}
		if len(got) != 1 || got[0] != owner {
			t.Errorf("%q is declared by %v, but stampFieldOwner names %s as its only owner. "+
				"scanStampWriters attributes a selector write by field name alone, so a second "+
				"declarer makes an unrelated assignment count as a writer of this field. "+
				"Either rename the new field, or teach the scan to resolve receiver types — "+
				"a sentence in stampFieldOwner's comment is not enough.", field, got, owner)
		}
	}
}
