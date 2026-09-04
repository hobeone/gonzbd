package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// manifestGateExempt lists the methods allowed to touch a manifest without
// routing through a residency gate, each with the reason.
//
// The polarity matters. A test that enumerates the methods which *must* be
// gated can only check the ones whoever wrote it remembered. This list is
// the inverse: every new method that touches a manifest fails until someone
// either gates it or writes down why it does not need to be. The list should
// only ever shrink.
//
// Keys are "Receiver.Method", not a bare method name.
var manifestGateExempt = map[string]string{
	"Job.AttachContent":         "assigns the manifest; establishes residency rather than relying on it",
	"Job.RestoreContent":        "assigns the manifest and validates alignment with progress; establishes residency rather than relying on it",
	"Job.Evict":                 "clears the manifest; runs precisely to drop residency",
	"Job.Resident":              "reports residency state directly",
	"Job.NumFiles":              "reads manifest count only if progress is not resident as a fallback",
	"Job.undeferRecovery":       "helper called only by gated callers (MarkArticleFailed, UndeferRecoveryVolumes)",
	"Job.DiscardDeferredPar2":   "updates fetch policy and recomputes progress against manifest if resident",
	"Job.ClearEmittedForReload": "sweeps articles to clear emitted state for reload; returns nil if manifest is not resident",
	"Job.ResetForRetry":         "carries deliberate early return for retry; both callers hydrate first, so the skip is unreachable and an error return would change an exported signature for a branch that cannot execute",
}

// TestManifestAccessIsGated asserts that every *Job method that dereferences
// a manifest field routes through a residency check (j.manifest == nil
// returning ErrNotResident) or is explicitly exempt.
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
			if !ok || fn.Body == nil || !isGatedReceiver(fn) {
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
			recv, name := receiverName(fn), fn.Name.Name
			key := recv + "." + name
			if _, exempt := manifestGateExempt[key]; exempt {
				seenExempt[key] = true
				continue
			}
			t.Errorf("(*%s).%s dereferences a manifest without going through a residency gate, so a non-resident job is skipped silently instead of reported (#261).\n"+
				"    Route it through j.manifest == nil returning ErrNotResident, or add %q to manifestGateExempt with the reason it does not need the gate.\n"+
				"    at %s", recv, name, key, fset.Position(fn.Pos()))
		}
	}

	if parsed == 0 {
		t.Fatal("parsed no non-test sources from the package directory; this test would pass vacuously")
	}

	if checked == 0 {
		t.Fatal("found no *Job method touching a manifest; the AST walk no longer matches the code's shape and this test would pass vacuously")
	}

	// A stale exemption is a slow leak back toward the ungated state: it
	// silently re-permits the next method that happens to take that name.
	for key := range manifestGateExempt {
		if !seenExempt[key] {
			t.Errorf("manifestGateExempt lists %q, but no ungated method by that name touches a manifest; remove the entry", key)
		}
	}
}

// receiverName returns the pointer-receiver type name of fn, or "" if fn is
// not a pointer-receiver method.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return ""
	}
	id, ok := star.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

// isGatedReceiver reports whether fn is a method on *Job.
func isGatedReceiver(fn *ast.FuncDecl) bool {
	return receiverName(fn) == "Job"
}

// manifestUse reports whether body reads a `.manifest` field and whether it
// routes through a residency gate.
//
// The manifest read is matched on the selector name alone.
// The gate is matched as a nil-check on manifest returning ErrNotResident or
// an error wrapping ErrNotResident (e.g. `j.manifest == nil`).
func manifestUse(body *ast.BlockStmt) (touches, gated bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if n.Sel.Name == "manifest" {
				touches = true
			}
		case *ast.IfStmt:
			// Check if the condition checks manifest == nil
			if isManifestNilCheck(n.Cond) && returnsErrNotResident(n.Body) {
				gated = true
			}
		}
		return true
	})
	return touches, gated
}

// isManifestNilCheck checks if an expression is `j.manifest == nil` or `j.manifest == nil || ...` or `... || j.manifest == nil`.
func isManifestNilCheck(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BinaryExpr:
		switch e.Op {
		case token.EQL:
			if isManifestExpr(e.X) && isNilIdent(e.Y) {
				return true
			}
			if isNilIdent(e.X) && isManifestExpr(e.Y) {
				return true
			}
		case token.LOR:
			return isManifestNilCheck(e.X) || isManifestNilCheck(e.Y)
		}
	}
	return false
}

func isManifestExpr(expr ast.Expr) bool {
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		return sel.Sel.Name == "manifest"
	}
	return false
}

func isNilIdent(expr ast.Expr) bool {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name == "nil"
	}
	return false
}

func returnsErrNotResident(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if ret, ok := n.(*ast.ReturnStmt); ok {
			for _, r := range ret.Results {
				ast.Inspect(r, func(rn ast.Node) bool {
					if ident, ok := rn.(*ast.Ident); ok && ident.Name == "ErrNotResident" {
						found = true
					}
					return true
				})
			}
		}
		return true
	})
	return found
}

// TestManifestGateCoversJobMethods pins that the gate's AST walk sees *Job methods.
func TestManifestGateCoversJobMethods(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	const src = `package job
func (j *Job) ungatedProbe() int { return j.manifest.NumFiles() }`
	file, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatalf("parse probe: %v", err)
	}
	fn, ok := file.Decls[0].(*ast.FuncDecl)
	if !ok {
		t.Fatalf("probe decl is %T, want *ast.FuncDecl", file.Decls[0])
	}

	if !isGatedReceiver(fn) {
		t.Fatal("isGatedReceiver rejected a *Job method")
	}
	touches, gated := manifestUse(fn.Body)
	if !touches || gated {
		t.Fatalf("manifestUse(ungated *Job probe) = (touches=%v, gated=%v), want (true, false)", touches, gated)
	}
}
