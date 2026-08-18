package queue

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// manifestGateExempt lists the *Queue methods allowed to touch job.manifest
// without routing through residentJob, each with the reason.
//
// The polarity matters. A test that enumerates the methods which *must* be
// gated can only check the ones whoever wrote it remembered — that is how
// MarkArticlesDone and MarkArticlesFailed stayed silent through the sweep
// that was supposed to convert them, and how the same shape made the phase
// test in #291 vacuous. This list is the inverse: every new method that
// touches a manifest fails until someone either gates it or writes down why
// it does not need to be. The list should only ever shrink.
var manifestGateExempt = map[string]string{
	"Add":                      "establishes the residency invariant rather than relying on it; assigns the manifest",
	"residentJob":              "is the gate",
	"hydrateJobLocked":         "loads the manifest; cannot require one to be present",
	"hydrateResidentLocked":    "delegates to hydrateJobLocked",
	"SnapshotJob":              "clones and hydrates; reads the field to decide whether hydration is needed",
	"PromoteNext":              "hydration machinery; runs before the job is resident",
	"Retry":                    "hydrates the job itself, then operates on the result",
	"undeferRecoveryLocked":    "takes an already-resolved *Job from a gated caller",
	"ClearAllEmitted":          "sweeps every job; skipping the non-resident ones is the correct behaviour, not a dropped request",
	"ForEachUnfinishedArticle": "enumerates dispatchable work across all jobs; a non-resident job has none to contribute",
}

// Every *Queue method that dereferences job.manifest must obtain its job
// through residentJob, so that a non-resident job is reported rather than
// silently skipped (#261). Exemptions are listed above with reasons.
func TestManifestAccessIsGated(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}

	fset := token.NewFileSet()
	var checked, parsed int
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
			if !ok || fn.Body == nil || !isQueueMethod(fn) {
				continue
			}
			touches, gated := manifestUse(fn.Body)
			if !touches {
				continue
			}
			checked++
			if gated {
				continue
			}
			if _, exempt := manifestGateExempt[fn.Name.Name]; exempt {
				seenExempt[fn.Name.Name] = true
				continue
			}
			t.Errorf("(*Queue).%s dereferences job.manifest without going through residentJob, so a non-resident job is skipped silently instead of reported (#261).\n"+
				"    Route it through residentJob, or add it to manifestGateExempt with the reason it does not need the gate.\n"+
				"    at %s", fn.Name.Name, fset.Position(fn.Pos()))
		}
	}

	if parsed == 0 {
		t.Fatal("parsed no non-test sources from the package directory; this test would pass vacuously")
	}

	if checked == 0 {
		t.Fatal("found no *Queue method touching job.manifest; the AST walk no longer matches the code's shape and this test would pass vacuously")
	}
	// A stale exemption is a slow leak back toward the ungated state: it
	// silently re-permits the next method that happens to take that name.
	for name := range manifestGateExempt {
		if !seenExempt[name] {
			t.Errorf("manifestGateExempt lists %q, but no ungated *Queue method by that name touches a manifest; remove the entry", name)
		}
	}
}

func isQueueMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Queue"
}

// manifestUse reports whether body reads a `.manifest` field and whether it
// calls residentJob. Both are matched on the selector name alone, which is
// sufficient here: `manifest` is not a field or method name on anything else
// in this package, and residentJob is a single unexported method.
func manifestUse(body *ast.BlockStmt) (touches, gated bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "manifest":
			touches = true
		case "residentJob":
			gated = true
		}
		return true
	})
	return touches, gated
}
