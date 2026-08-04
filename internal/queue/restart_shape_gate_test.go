package queue

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// rowShapeExempt lists the functions that read job_files without checking the
// rows against the stored manifest, each with the reason they do not need to.
//
// The polarity is the same as manifestGateExempt's, and for the same reason: a
// list of the readers that *must* check can only cover the ones whoever wrote
// it remembered. This list is the inverse — a new reader of job_files fails
// until someone either range-checks its indices against the manifest or writes
// down why the read cannot be shifted by a renumber. The list should only ever
// shrink.
//
// What makes a reader safe is that it never binds a row to a file by position.
// Aggregates, counts and bulk copies survive a renumber because they do not
// care which file a row belongs to; anything that indexes progress.files by a
// stored file_index does not.
var rowShapeExempt = map[string]string{
	"storedFileRows":      "returns rows verbatim and binds nothing; every caller either validates the indices (RestoreJobProgress) or remaps by subject (finishInterruptedRewrite)",
	"ArticleCountsByJob":  "places counts by file_index into a map keyed the same way, so a renumber shifts the map identically rather than mismatching it",
	"RemainingBytesByJob": "sums bytes per job_id; no per-file binding",
	"MoveToHistory":       "copies rows wholesale into history_job_files, preserving whatever shape they have",
}

// Every function that reads job_files must either range-check the rows against
// the stored manifest — reporting ErrManifestStale when they disagree — or be
// listed above with the reason it cannot be affected.
//
// The manifest blob and the job_files rows are written by separate,
// non-atomic operations (Store.ReplaceManifest), so a crash can leave them
// describing different file sets. Inside one process manifestRowsStale (#310)
// records that; across a restart nothing does, and the row indices are the
// only surviving evidence. A reader that silently skips an out-of-range index
// discards that evidence and lets the in-range rows bind to the wrong files.
func TestJobFilesReadsCheckRowShape(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}

	fset := token.NewFileSet()
	var parsed, checked int
	seenExempt := make(map[string]bool)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			reads, guarded := jobFilesRead(fn.Body)
			if !reads {
				continue
			}
			checked++
			if guarded {
				continue
			}
			if _, exempt := rowShapeExempt[fn.Name.Name]; exempt {
				seenExempt[fn.Name.Name] = true
				continue
			}
			t.Errorf("%s reads job_files without any reference to ErrManifestStale, so nothing here considers whether the stored rows still match the stored manifest, so a file_index left over from a torn ReplaceManifest binds to the wrong file (#310).\n"+
				"    Range-check the index and return ErrManifestStale, or add it to rowShapeExempt with the reason a renumber cannot affect it.\n"+
				"    at %s", fn.Name.Name, fset.Position(fn.Pos()))
		}
	}

	if parsed == 0 {
		t.Fatal("parsed no non-test sources from the package directory; this test would pass vacuously")
	}
	if checked == 0 {
		t.Fatal("found no function reading job_files; the AST walk no longer matches the code's shape and this test would pass vacuously")
	}
	for name := range rowShapeExempt {
		if !seenExempt[name] {
			t.Errorf("rowShapeExempt lists %q, but no unguarded function by that name reads job_files; remove the entry", name)
		}
	}
}

// jobFilesRead reports whether body contains a SELECT against job_files and
// whether it references ErrManifestStale.
//
// Matching the query on its text rather than parsing SQL is deliberate: the
// queries are constant strings in this package, and a reader that builds one
// dynamically is a change worth failing on rather than accommodating.
// "FROM history_job_files" does not contain "FROM job_files", so the history
// table's own readers are excluded without a special case.
//
// What this does not catch, stated so the gate is not mistaken for more than
// it is: it keys on the query site, so a function that binds rows obtained
// from a helper is invisible to it (which is why storedFileRows carries the
// obligation forward in its exemption reason rather than being waved
// through); a `JOIN job_files` would not match; and any mention of
// ErrManifestStale counts as a check, so it proves the author considered the
// disagreement, not that they handled it correctly. It is a tripwire against
// a new reader appearing unexamined, not a proof of correctness.
func jobFilesRead(body *ast.BlockStmt) (reads, guarded bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			// A DELETE names the table the same way but binds nothing to a
			// file, so it cannot be shifted by a renumber. Remove those
			// occurrences before asking whether a read remains; a literal
			// carrying both still counts as a read.
			if node.Kind == token.STRING {
				q := strings.ReplaceAll(node.Value, "DELETE FROM job_files", "")
				if strings.Contains(q, "FROM job_files") {
					reads = true
				}
			}
		case *ast.Ident:
			if node.Name == "ErrManifestStale" {
				guarded = true
			}
		}
		return true
	})
	return reads, guarded
}
