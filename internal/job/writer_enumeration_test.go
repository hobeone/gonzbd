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

// fieldOwner names the struct type that declares each field one of this
// package's writer-enumeration tests scans for. scanWriters uses it to tell
// a keyed `field: x` composite-literal element that legitimately sets the
// field apart from an unrelated struct that happens to reuse the same field
// name — outcome, next and crossed are Attempt's; attempts, intent and lease
// are Job's. The claim that no OTHER struct here declares any of these six
// names is established by reading the declarations, not by grepping the
// names: `git grep -n '^typ[e] [A-Za-z]* struct' -- 'internal/job/*.go'
// ':!internal/job/*_test.go'` returns six types — Attempt, Job, Lease, Policy,
// RenderView, StateView — and only the first two declare any of these fields.
// A grep for the field names themselves returns ~50 lines of comments and
// error strings that no reader can filter into an answer, which is why it is
// not the citation. Matching on the field name alone would therefore still be
// correct today; this table exists so that stops being an accident if a
// seventh type is added later.
var fieldOwner = map[string]string{
	"outcome":  "Attempt",
	"next":     "Attempt",
	"crossed":  "Attempt",
	"attempts": "Job",
	"intent":   "Job",
	"lease":    "Job",
}

// scanWriters parses this package's non-test sources and returns the sorted,
// deduplicated names of the functions that write to the unexported field
// named field, via:
//
//   - a plain or compound `=` assignment to `x.field` or `x.field[i]`
//     (`a.next = ...`, `a.outcome += ...`, `j.attempts[i] = ...` — the
//     IndexExpr arm is what makes the last of those visible, since
//     `j.attempts[i] = ...`'s left side is an IndexExpr whose .X is the
//     SelectorExpr, not a SelectorExpr itself);
//   - `x.field++` / `x.field--`;
//   - a keyed `field: x` element in a composite literal whose type is
//     fieldOwner[field]. An UNKEYED literal of that type with elements fails
//     the test outright rather than being silently miscounted — this scan
//     cannot tell which field a positional element sets, and refusing it
//     outright is cheaper and safer than resolving the type's field order to
//     a positional index.
//
// It reads the directory rather than taking a file list, and inspects each
// file's whole AST rather than only *ast.FuncDecl bodies, so a source file
// added later, and a package-level `var x = Attempt{field: ...}` inside it,
// are both covered without anyone remembering to add either here — the
// failure mode every test built on this scanner exists to close is a claim
// that went stale because nobody opened the file that falsified it. A write
// found outside any enclosing function (a package-level var's initializer)
// is attributed to a synthetic "var <name> (package-level)" label rather
// than a function name, so it still shows up as an unexpected entry in the
// writer set instead of being silently dropped for lack of a function to
// blame.
//
// This factors what were, before this task, two independently-maintained
// copies — scanOutcomeWriters (outcome, on Attempt) and scanAttemptsWriters
// (attempts, on Job) — into one implementation parameterized on field. Both
// call this now, and TestCrossedWrites/TestIntentWrites/TestNextWrites/
// TestLeaseWrites below are the four new callers Task 7 adds.
func scanWriters(t *testing.T, field string) []string {
	t.Helper()

	owner := fieldOwner[field]

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
				// form (+=, |=, ...) — is a write to whatever selector or
				// index expression it targets.
				if node.Tok == token.DEFINE {
					return true
				}
				for _, lhs := range node.Lhs {
					// A plain `a.field = ...` is a SelectorExpr on the
					// left. `j.attempts[i] = ...` is an IndexExpr whose .X
					// is that same SelectorExpr — the SelectorExpr case
					// alone never matches an IndexExpr node.
					sel, ok := lhs.(*ast.SelectorExpr)
					if idx, isIndex := lhs.(*ast.IndexExpr); isIndex {
						sel, ok = idx.X.(*ast.SelectorExpr)
					}
					if ok && sel.Sel.Name == field {
						writers = append(writers, label)
					}
				}
			case *ast.IncDecStmt:
				// a.field++ / a.field-- write the field without being an
				// AssignStmt at all.
				if sel, ok := node.X.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
					writers = append(writers, label)
				}
			case *ast.CompositeLit:
				// &Attempt{field: x} / Job{field: x} sets the field
				// without an AssignStmt at all — the ASSIGN-only scan
				// above cannot see it. Only a literal whose type is
				// fieldOwner[field] counts: a bare Ident type check, not
				// "any composite literal with a key or element named
				// field" — otherwise an unrelated struct that happens to
				// have its own field called field would be misattributed.
				// An elided type — the inner literal of []Attempt{{...}} or
				// map[K]Attempt{k: {...}} — parses with node.Type == nil.
				// Skipping those silently is the hole this scan exists to
				// close: a write through one would not be counted and the
				// enumeration would pass while a second writer existed. The
				// type cannot be resolved without go/types, so attribute
				// nothing and fail loudly instead, but only when the literal
				// actually names the field — transition.go's legalEdges rows
				// ({Assessing}) are elided too and name nothing.
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
				// An unkeyed literal sets fields by position, including
				// field, without any key this scan can match against — it
				// would slip past silently rather than being (correctly)
				// counted as a write.
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

	// A parse that silently matched nothing would report an empty list and
	// read as "field is never written" rather than "the scan broke".
	if scanned == 0 {
		t.Fatal("no non-test sources parsed; the scan found nothing to check " +
			"and its empty result would otherwise look like a real answer")
	}
	if len(writers) == 0 {
		t.Fatalf("scan found zero writers of %s; an empty result reads identically to "+
			"\"nobody writes it\" and means the scan itself is broken, not that the "+
			"population is empty", field)
	}

	slices.Sort(writers)
	return slices.Compact(writers)
}

// TestCrossedWrites_MatchTheEnumerationStatedInProse asserts cross is the
// sole writer of a.crossed. Before this design, transition wrote it; the
// boundary door now owns it, and the whole argument for Cross existing is
// that entering Production and surrendering the lease cannot be separated. A
// second writer would separate them again.
func TestCrossedWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanWriters(t, "crossed")
	want := []string{"cross"}
	if !slices.Equal(writers, want) {
		t.Errorf("functions assigning crossed = %v, want %v\n\n"+
			"the crossed field's doc comment (attempt.go) claims cross is the sole writer "+
			"of this latch. If a second writer is correct, say so at that comment AND "+
			"update this list together — a comment that still says \"the sole writer\" "+
			"once a second one exists is worse than no comment at all.",
			writers, want)
	}
}

// TestIntentWrites_MatchTheEnumerationStatedInProse asserts SetIntent is the
// sole writer of j.intent, which is what makes the cancel latch a property
// rather than a convention.
func TestIntentWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanWriters(t, "intent")
	want := []string{"SetIntent"}
	if !slices.Equal(writers, want) {
		t.Errorf("functions assigning intent = %v, want %v\n\n"+
			"the intent field's doc comment (job.go) claims SetIntent is its sole writer. "+
			"If a second writer is correct, say so at that comment AND update this list "+
			"together — a comment that still says \"sole writer\" once a second one exists "+
			"is worse than no comment at all.",
			writers, want)
	}
}

// TestNextWrites_MatchTheEnumerationStatedInProse asserts the four writers
// of a.next and no others: setNext records the marker, transition and cross
// each clear it when they take the move, and finish clears it alongside
// activity when it settles the attempt. A fifth writer is how a stale
// verdict survives a state change.
func TestNextWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanWriters(t, "next")
	want := []string{"cross", "finish", "setNext", "transition"}
	if !slices.Equal(writers, want) {
		t.Errorf("functions assigning next = %v, want %v\n\n"+
			"setNext's doc comment (attempt.go) claims these four functions, and only "+
			"these four, ever write a.next. If a different writer is correct, say so at "+
			"that comment AND update this list together.",
			writers, want)
	}
}

// TestLeaseWrites_MatchTheEnumerationStatedInProse asserts Grant and
// surrenderLocked are the only writers of j.lease. surrenderLocked being the
// sole RELEASER is what lets Cross and Finish yield without reacquiring
// j.mu — see surrenderLocked's own doc comment for why the exported
// Surrender would deadlock if called from either door.
func TestLeaseWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanWriters(t, "lease")
	want := []string{"Grant", "surrenderLocked"}
	if !slices.Equal(writers, want) {
		t.Errorf("functions assigning lease = %v, want %v\n\n"+
			"the lease field's doc comment (job.go) claims Grant and surrenderLocked are "+
			"its only writers. If a different writer is correct, say so at that comment "+
			"AND update this list together.",
			writers, want)
	}
}
