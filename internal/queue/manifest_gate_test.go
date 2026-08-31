package queue

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
// gated can only check the ones whoever wrote it remembered — that is how
// MarkArticlesDone and MarkArticlesFailed stayed silent through the sweep
// that was supposed to convert them, and how the same shape made the phase
// test in #291 vacuous. This list is the inverse: every new method that
// touches a manifest fails until someone either gates it or writes down why
// it does not need to be. The list should only ever shrink.
//
// Keys are "Receiver.Method", not a bare method name. Two receivers are
// walked now, and a bare name would let an exemption written for one silently
// cover a same-named method on the other — the aliasing the stale-entry check
// at the bottom of TestManifestAccessIsGated exists to prevent, reintroduced
// through the key.
var manifestGateExempt = map[string]string{
	"Queue.Add":                      "establishes the residency invariant rather than relying on it; assigns the manifest",
	"Queue.hydrateJobLocked":         "loads the manifest; cannot require one to be present",
	"Queue.hydrateResidentLocked":    "delegates to hydrateJobLocked",
	"Queue.SnapshotJob":              "clones and hydrates; reads the field to decide whether hydration is needed",
	"Queue.PromoteNext":              "hydration machinery; runs before the job is resident",
	"Queue.Retry":                    "hydrates the job itself, then operates on the result",
	"Queue.undeferRecoveryLocked":    "takes an already-resolved *Job from a gated caller",
	"Queue.ClearAllEmitted":          "sweeps every job; skipping the non-resident ones is the correct behaviour, not a dropped request",
	"Queue.ForEachUnfinishedArticle": "enumerates dispatchable work across all jobs; a non-resident job has none to contribute",

	// Job.resident is the gate itself, and carries the entry that used to sit
	// on Queue.residentJob: the condition moved, so the exemption moved with
	// it. residentJob now delegates and no longer names a manifest at all,
	// which is why leaving its entry in place failed the stale-entry check
	// rather than passing quietly.
	"Job.resident": "is the gate",

	// The remaining *Job entries all predate B2.4a. Widening the walk to *Job
	// is what made them visible; none is a method this change introduced.
	"Job.Manifest":          "IS the residency report — the fallible accessor docs/queue-lifecycle.md § Enforcement requires; gating it on residency would make it unable to say what it exists to say",
	"Job.setResidency":      "assigns the manifest; establishes residency rather than relying on it",
	"Job.setHydrateFailure": "clears the manifest and records why; runs precisely when there is none",
	"Job.ResetForRetry":     "carries #261's one deliberately unconverted early return, with the argument at its declaration: both callers hydrate first, so the skip is unreachable and an error return would change an exported signature for a branch that cannot execute",
	"Job.MarshalJSON":       "serialises whichever tiers are present; a non-resident job marshals a null manifest by design",
	"Job.UnmarshalJSON":     "constructs the manifest; cannot require one to be present",
}

// Every *Queue or *Job method that dereferences a manifest must route through
// a residency gate — Queue.residentJob or Job.resident — so that a
// non-resident job is reported rather than silently skipped (#261).
// Exemptions are listed above with reasons.
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
			key := receiverName(fn) + "." + fn.Name.Name
			if _, exempt := manifestGateExempt[key]; exempt {
				seenExempt[key] = true
				continue
			}
			t.Errorf("(*%s) dereferences a manifest without going through a residency gate, so a non-resident job is skipped silently instead of reported (#261).\n"+
				"    Route it through Queue.residentJob or Job.resident, or add %q to manifestGateExempt with the reason it does not need the gate.\n"+
				"    at %s", key, key, fset.Position(fn.Pos()))
		}
	}

	if parsed == 0 {
		t.Fatal("parsed no non-test sources from the package directory; this test would pass vacuously")
	}

	if checked == 0 {
		t.Fatal("found no *Queue or *Job method touching a manifest; the AST walk no longer matches the code's shape and this test would pass vacuously")
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

// isGatedReceiver reports whether fn is a method on *Queue or *Job.
//
// Both are in scope because the manifest tier is moving from the first to
// the second (B2.4a), and a walk that matched only *Queue would go blind
// exactly as it moved — see TestManifestGateCoversJobMethods for why that
// would not show up as a failure.
func isGatedReceiver(fn *ast.FuncDecl) bool {
	switch receiverName(fn) {
	case "Queue", "Job":
		return true
	default:
		return false
	}
}

// manifestUse reports whether body reads a `.manifest` field and whether it
// routes through a residency gate. Both are matched on the selector name
// alone, which is sufficient here: `manifest` is not a field or method name
// on anything else in this package, and the two gates are single unexported
// methods — Queue.residentJob, which looks a job up and checks it, and
// Job.resident, which checks the job it is called on.
func manifestUse(body *ast.BlockStmt) (touches, gated bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "manifest":
			touches = true
		case "residentJob", "resident":
			gated = true
		}
		return true
	})
	return touches, gated
}

// TestManifestGateCoversJobMethods pins that the gate's AST walk sees *Job
// methods, not only *Queue ones.
//
// B2.4a moves the manifest-tier bodies from *Queue onto *Job. A walk that
// still matched only *Queue would keep PASSING while covering none of them:
// TestManifestAccessIsGated's vacuity guards check that SOME *Queue method
// touches a manifest, and several always will (Add, PromoteNext, Retry), so
// the gate would go blind without going red. That is the #261 protection
// evaporating silently, which is the one failure mode a gate must not have.
//
// The probe is a synthetic source string rather than a real method name, so
// this test cannot go stale when the package's methods move or are renamed.
func TestManifestGateCoversJobMethods(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	const src = `package queue
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
		t.Fatal("isGatedReceiver rejected a *Job method, so the gate cannot see the tier B2.4a moves onto Job")
	}
	touches, gated := manifestUse(fn.Body)
	if !touches || gated {
		t.Fatalf("manifestUse(ungated *Job probe) = (touches=%v, gated=%v), want (true, false)", touches, gated)
	}
}
