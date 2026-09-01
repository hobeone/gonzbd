package queue

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

// downloadStampWriters is the enumeration these tests defend: the functions
// permitted to assign p.downloadStarted or p.downloadFinished by name.
//
// The rule the owner enforces — a job timestamp's Unix() must be positive,
// because the store's integer column reads 0 as absent — is stated at
// isJobStamp in progress.go and settled on #464. A fifth writer could store a
// value that rule forbids, and nothing else in the build would notice: the
// fields are unexported, so no compiler error follows, and the store would
// simply persist a stamp it later decodes as "never finished".
//
// SCOPE — three exclusions, all deliberate:
//
//   - Whole-struct copy. JobProgress.clone does `cp := *p`, which propagates
//     both fields and mints neither. It is cited by name rather than by line
//     because #464 inserted ~90 lines above it in the same file.
//   - Test sources. The scan skips *_test.go, and more than one of them writes
//     these fields directly: challenger_m3_test.go assigns downloadFinished by
//     selector, and progress_test.go builds a keyed JobProgress literal that
//     the composite-literal arm would otherwise match. Each has its own
//     justification at the site.
//   - Field identity. The AssignStmt and IncDecStmt arms match on the selector
//     NAME alone, with no go/types resolution, so a second struct declaring a
//     field of either name would poison the set.
//     TestStampFieldOwners_AreTheOnlyDeclarers is what closes that, because a
//     sentence here would fail silently and point at the wrong file.
//
// ASSERTED PER FIELD, NOT AS A UNION. A single four-name set is strictly
// weaker: setDownloadFinishedOnce miswired to write downloadStarted still
// produces exactly those four names. Two three-name sets catch that here, as
// TestSetDownloadStampOnce_FirstWinsAndRefusesNonStamps's paired getters catch
// it behaviourally.
var downloadStartedWriters = []string{
	"clearDownloadStamps", "restoreDownloadStamps", "setDownloadStartedOnce",
}

var downloadFinishedWriters = []string{
	"clearDownloadStamps", "restoreDownloadStamps", "setDownloadFinishedOnce",
}

// scanStampWriters parses this package's non-test sources and returns the
// sorted, deduplicated names of the functions that write the unexported field
// named field, via:
//
//   - a plain or compound `=` assignment to `x.field` or `x.field[i]`,
//     parentheses unwrapped;
//   - `x.field++` / `x.field--`;
//   - `&x.field`, because a pointer to the field can be written through as
//     `*ptr = t`, which names no field for any scan to match. Counting the
//     address-of catches it at the last point the field is still named, and
//     deliberately over-reports: taking the address to read counts too.
//   - a keyed `field: x` element in a composite literal whose type is
//     stampFieldOwner[field]. An UNKEYED literal of that type with elements
//     fails outright rather than being silently miscounted — the scan cannot
//     tell which field a positional element sets.
//
// Three escapes an earlier draft of this file had, all found by review rather
// than by any gate, and all closed above: the parenthesized left-hand side,
// the address-of, and `type JP = JobProgress` (refused by
// TestStampFieldOwners_AreTheOnlyDeclarers, since the composite-literal arm
// matches a literal's type by NAME). The one that remains is reflection, which
// this package does not use on JobProgress and which no AST scan can see.
//
// It is modelled on internal/job/writer_enumeration_test.go's scanWriters,
// which is unexported and scoped to that package's own directory, so this is
// the pattern copied rather than the function called. The other enumeration in
// this package, scanDoneBitWriters in donebit_enumeration_test.go, matches CALL
// expressions and returns two lists; sharing a directory walk between the two
// would entangle their matching logic for a test-time-only saving, so they
// parse the package separately.
//
// It reads the directory rather than taking a file list, and inspects whole
// ASTs rather than only *ast.FuncDecl bodies, so a source file added later —
// and a package-level `var x = JobProgress{downloadStarted: ...}` inside it —
// are both covered without anyone remembering to come back here.
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

		// label is what to attribute a write to: the enclosing function's
		// name inside a FuncDecl body, or a synthetic package-level label
		// while walking a GenDecl's values, which have no enclosing function.
		var label string
		record := func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				// `:=` cannot target an existing selector, so every
				// remaining token — ASSIGN and every compound form — writes
				// whatever selector or index expression it targets.
				if node.Tok == token.DEFINE {
					return true
				}
				for _, lhs := range node.Lhs {
					// ast.Unparen: `(p.downloadStarted) = t` is legal Go and
					// parses as a ParenExpr, which no type assertion below
					// matches. Dropping it would be a silent miss.
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
				// &p.downloadStarted hands the field to something that can
				// write it through the pointer, and the write itself is then
				// `*ptr = t` — a StarExpr naming no field, which nothing here
				// can match. The escape is closed at the address-of instead,
				// which is the last point the field is still named.
				//
				// This deliberately over-reports: taking the address to READ
				// counts as a writer too. That direction is the safe one — it
				// fails loudly and asks for a decision, where the alternative
				// misses a real write in silence.
				if node.Op != token.AND {
					return true
				}
				if sel, ok := ast.Unparen(node.X).(*ast.SelectorExpr); ok && sel.Sel.Name == field {
					writers = append(writers, label)
				}
			case *ast.CompositeLit:
				// &JobProgress{downloadStarted: t} sets the field with no
				// AssignStmt at all, which an ASSIGN-only scan cannot see.
				// An elided type — the inner literal of []JobProgress{{...}}
				// — parses with node.Type == nil and cannot be resolved
				// without go/types, so fail loudly rather than drop the
				// write, but only when the literal actually names the field.
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

	// Either of these empty results reads identically to a real answer —
	// "nothing writes this field" — while meaning the scan itself broke.
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

// TestDownloadStampWriters_MatchTheEnumerationStatedInProse is what replaces
// the hand-run grep that progress.go's owner comment carried while #464 was in
// flight. A citation states a count a reader must re-derive; this fails when
// the set moves.
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
					"setDownloadStartedOnce's doc comment (progress.go) claims the four owner "+
					"methods are the only functions in this package's non-test sources that "+
					"assign either stamp by name, which is what makes #464's rule — a job "+
					"timestamp's Unix() must be positive — an invariant rather than a "+
					"convention. If a new writer is correct, it must apply isJobStamp itself; "+
					"say so at that comment AND update this list together.",
					tc.field, writers, tc.want)
			}
		})
	}
}

// TestStampFieldOwners_AreTheOnlyDeclarers enforces the population
// stampFieldOwner merely asserts.
//
// scanStampWriters is asymmetric, and this closes the gap. Its CompositeLit
// arm checks the literal's type, so an unrelated `Foo{downloadStarted: x}` is
// ignored. Its AssignStmt and IncDecStmt arms match the selector NAME alone —
// `anything.downloadStarted = x` counts, whatever `anything` is — because
// resolving the receiver's type needs go/types, which this scan does not run.
// That is sound exactly as long as no other struct in the package declares
// either name.
//
// It is a test rather than a sentence because the population is
// machine-enumerable and a sentence fails silently: a struct added tomorrow
// with its own downloadStarted field would make an unrelated assignment appear
// as a fourth writer, failing an ownership test in a file nobody touched, with
// an error message pointing at the wrong thing entirely.
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
			// `type JP = JobProgress` defeats both tests at once: the
			// composite-literal arm of scanStampWriters compares the
			// literal's type name against stampFieldOwner, so JP{...} is
			// skipped, and this test walks StructType nodes, which an alias
			// is not. Refuse the alias rather than resolve it — resolution
			// needs go/types, and the package has no alias today.
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
