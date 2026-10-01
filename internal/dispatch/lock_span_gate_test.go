package dispatch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// queueCallAllow lists calls found inside a d.mu-held span that
// TestNoCallIntoQueueUnderDispatcherLock cannot resolve to a package-local
// function it can inspect: a call through one of the Dispatcher's other
// interface fields (res, store, runner), its func-typed beforeClaim field, or
// a func-typed parameter of the enclosing function. Each entry names why that
// call cannot reach sched.Queue.
//
// The polarity matters, the same way it does in manifestGateExempt
// (manifest_gate_test.go). This is not a list of calls known to be safe
// anywhere in the package; it is the inverse — every call of this SHAPE found
// inside a held span fails until someone either moves it out of the span or
// writes down here why it cannot reach the Queue. The list should only ever
// shrink, and a stale entry (naming a call the scan no longer finds inside
// any span) is itself a failure below.
var queueCallAllow = map[string]string{}

// callKind classifies one call found while scanning a d.mu-held span.
type callKind int

const (
	callIgnored callKind = iota
	callQueue
	callMaybeLocal
	callUnresolved
)

// selectorPath flattens a chain of selectors down to a dotted identifier
// path, e.g. `d.q.Advance` -> []string{"d", "q", "Advance"}. It returns nil
// for any base it cannot resolve to a plain identifier (an index expression,
// a call result, and so on), which classifyCall then treats as ignored —
// none of those shapes appear as a call target in this package today (see
// the citations in classifyMember's comment, below).
func selectorPath(e ast.Expr) []string {
	switch v := e.(type) {
	case *ast.Ident:
		return []string{v.Name}
	case *ast.SelectorExpr:
		base := selectorPath(v.X)
		if base == nil {
			return nil
		}
		return append(base, v.Sel.Name)
	default:
		return nil
	}
}

// dispatcherSafeFields names the Dispatcher fields whose two-level call
// `d.<field>.<method>(...)` classifyMember ignores outright, because the
// field's own type cannot reach sched.Queue. Each reason is why, so a
// reviewer does not have to re-derive it; a field listed here that no call
// site in the package actually exercises any more is itself a failure (see
// TestNoCallIntoQueueUnderDispatcherLock's trailing loop over this map),
// the same way a stale queueCallAllow entry is.
//
// Enumerated from the same scan classifyMember's own comment cites below:
// of the two-level bases that scan finds (mu, storeMu, q, res, store,
// runner, log, stopOnce), q is handled by its own case, and res/store/runner
// are deliberately ABSENT here — they are the interfaces this gate exists to
// catch, so they must fall through to the catch-all callUnresolved case
// rather than be declared safe.
var dispatcherSafeFields = map[string]string{
	"mu":       "sync.Mutex; muField intercepts every Lock/RLock/Unlock/RUnlock call on it before classifyCall ever runs in scanFuncForLockedQueueCalls, and a mutex has no other exported method a CallExpr could target",
	"storeMu":  "a second, unrelated sync.Mutex guarding store writes; same reasoning as mu, and it is never read inside a d.mu span (TestStoreMuLockNesting_MatchesEnumeration, lock_enumeration_test.go, pins the one place the two nest)",
	"log":      "*slog.Logger; its methods only format and emit a record, never call back into this package",
	"stopOnce": "sync.Once; the call on stopOnce itself is just Do(closure) — the closure's own calls are inspected separately, as sibling nodes of the same ast.Inspect walk, not skipped by ignoring Do",
}

// classifyMember interprets rest — a selector path with a receiver of static
// type recvType ("Dispatcher" or "removal") already stripped off its front —
// as a field or method access on that type.
//
// The cases below are not a guess at Dispatcher's shape: a package-wide scan
// of every `d.<x>(` and `d.<x>.<y>(` and `r.d...` call —
// `grep -noE 'd\.[A-Za-z_]+\(' internal/dispatch/*.go | grep -v _test.go`
// and the `d\.[A-Za-z_]+\.[A-Za-z_]+\(` and `r\.d\.` variants — finds exactly
// these shapes: every bare `d.<name>(` is either a real *Dispatcher method or
// the func-typed beforeClaim field, and every `d.<field>.<method>(` reaches
// through one of q, res, store, runner, log, mu, storeMu or stopOnce. Of
// those, mu, storeMu, log and stopOnce are declared safe in
// dispatcherSafeFields; q gets its own case; everything else two levels deep
// — res, store, runner today, and any field this package adds later that is
// itself callable this same way — falls to the final callUnresolved case, so
// a NEW two-level call through a field is flagged for review rather than
// silently falling through as callIgnored.
func classifyMember(recvType string, rest []string) (kind callKind, ident string) {
	switch recvType {
	case "Dispatcher":
		switch {
		case len(rest) == 2 && rest[0] == "q":
			return callQueue, "d.q." + rest[1] + "(...)"
		case len(rest) == 1 && rest[0] == "beforeClaim":
			return callUnresolved, "d.beforeClaim(...)"
		case len(rest) == 2:
			if _, safe := dispatcherSafeFields[rest[0]]; safe {
				return callIgnored, rest[0]
			}
			return callUnresolved, "d." + rest[0] + "." + rest[1] + "(...)"
		case len(rest) == 1:
			return callMaybeLocal, "Dispatcher." + rest[0]
		}
	case "removal":
		switch {
		case len(rest) >= 2 && rest[0] == "d":
			// The removal's own *Dispatcher field: r.d.X(...) reaches exactly
			// what d.X(...) would from a *Dispatcher method, so recurse with
			// the "d" hop stripped — this is what makes r.d.deregister(...)
			// (registry.go, removal.end) resolve to Dispatcher.deregister,
			// and r.d.mu.Lock()/Unlock() (registry.go, removal.abort) resolve
			// to the same safe "mu" case above.
			return classifyMember("Dispatcher", rest[1:])
		case len(rest) == 1:
			return callMaybeLocal, "removal." + rest[0]
		}
		// No other shape is reachable: removal's only fields are d
		// (*Dispatcher, handled above), id (string) and done (bool)
		// (registry.go's `type removal struct`), and neither of the latter
		// two has a method a CallExpr could target.
	}
	return callIgnored, ""
}

// classifyCall classifies one CallExpr found anywhere in the package: a
// direct call into sched.Queue, a call this test can follow into another
// package-local function's body (local to Dispatcher, local to removal, or a
// bare package-level function), a call through something it cannot follow
// (an interface field or a func-typed value), or anything else.
//
// recv and recvType are the enclosing function's own receiver variable and
// type ("d"/"Dispatcher", "r"/"removal", or "" for a bare function).
// fvParams is the set of the enclosing function's OWN parameters whose type
// is a func signature (built by funcValueParamNames) — this is what makes
// `fn(occupyCtx)` in occupyFor and `beforeFirstTick(ctx)` in StartWith
// unresolved without hard-coding either name.
func classifyCall(call *ast.CallExpr, recv, recvType string, fvParams map[string]bool) (kind callKind, ident string) {
	path := selectorPath(call.Fun)
	if path == nil {
		return callIgnored, ""
	}
	head := path[0]
	rest := path[1:]

	if fvParams[head] {
		return callUnresolved, strings.Join(path, ".") + "(...)"
	}
	if recv != "" && head == recv {
		return classifyMember(recvType, rest)
	}
	if (head == "r" || head == "rm") && head != recv {
		return classifyMember("removal", rest)
	}
	if len(path) == 1 {
		return callMaybeLocal, head // resolved against funcs by the caller; a miss is simply not flagged
	}
	return callIgnored, ""
}

// funcKey names a declaration the way classifyCall's targets are spelled:
// "Type.Method" for a pointer-receiver method, or the bare name for a
// function with no receiver.
func funcKey(fn *ast.FuncDecl) string {
	if t := receiverType(fn); t != "" {
		return t + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// receiverVar returns a method's receiver variable name, or "" for a
// function with no receiver.
func receiverVar(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 || len(fn.Recv.List[0].Names) == 0 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// receiverType returns a method's pointer-receiver type name, or "" for a
// function with no receiver.
func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// funcValueParamNames returns the names of fn's own parameters declared with
// an inline func signature — computed from the declaration rather than
// hard-coded, so a future parameter of this shape is picked up automatically
// instead of silently falling through as an ordinary resolved call.
func funcValueParamNames(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn.Type == nil || fn.Type.Params == nil {
		return out
	}
	for _, field := range fn.Type.Params.List {
		if _, ok := field.Type.(*ast.FuncType); !ok {
			continue
		}
		for _, name := range field.Names {
			out[name.Name] = true
		}
	}
	return out
}

// muField reports the lock field name ("mu", "storeMu", or "" for anything
// else) a Lock/RLock/Unlock/RUnlock call targets. The match looks only at the
// immediate base of the call's selector, mirroring
// TestStoreMuLockNesting_MatchesEnumeration's analyzeLockNesting
// (lock_enumeration_test.go) — which is why `d.mu.Lock()` and the removal
// type's `r.d.mu.Lock()` are both recognized regardless of how deep the
// receiver chain runs underneath the final ".mu".
func muField(call *ast.CallExpr) (field, verb string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	verb = sel.Sel.Name
	switch verb {
	case "Lock", "RLock", "Unlock", "RUnlock":
	default:
		return "", "", false
	}
	switch inner := sel.X.(type) {
	case *ast.SelectorExpr:
		return inner.Sel.Name, verb, true
	case *ast.Ident:
		return inner.Name, verb, true
	}
	return "", "", false
}

// deferredCalls returns the set of CallExprs inside body that are the target
// of a defer statement, the same pre-pass analyzeLockNesting uses: a deferred
// Unlock does not release the lock at that point in the text, only at the
// function's return.
func deferredCalls(body *ast.BlockStmt) map[*ast.CallExpr]bool {
	out := map[*ast.CallExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if d, ok := n.(*ast.DeferStmt); ok {
			out[d.Call] = true
		}
		return true
	})
	return out
}

// parseDispatchSources parses every non-test .go file in the package
// directory and returns every top-level function and method declaration,
// keyed by funcKey.
func parseDispatchSources(t *testing.T) (*token.FileSet, map[string]*ast.FuncDecl) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	parsed := 0
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
			funcs[funcKey(fn)] = fn
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no non-test sources from the package directory; this test would pass vacuously")
	}
	if len(funcs) == 0 {
		t.Fatal("found no function declarations; the AST walk no longer matches the code's shape")
	}
	return fset, funcs
}

// packageAnalysis is the one full, lock-unaware walk over every function
// body that TestNoCallIntoQueueUnderDispatcherLock needs before it can scan
// any d.mu span.
type packageAnalysis struct {
	// reaches[key] is whether that function calls into sched.Queue directly
	// or, transitively, through another function this test can resolve
	// (callMaybeLocal).
	reaches map[string]bool
	// unresolvedReach[key] lists every distinct unresolved-call description
	// (an interface field or func value classifyCall could not follow)
	// reachable from that function, directly or through a resolved local
	// callee — so a one-line helper's d.runner.Run(...) still surfaces at
	// every call site that reaches the helper, not just inside the helper
	// itself.
	unresolvedReach map[string][]string
	// safeFieldsUsed[field] is whether some call anywhere in the package
	// actually matched that entry of dispatcherSafeFields.
	safeFieldsUsed map[string]bool
}

// analyzePackage walks every function in funcs exactly once. The walk is NOT
// scoped to a d.mu span — reachability (of sched.Queue, and of an unresolved
// call) is a property of what a function calls anywhere in its body, because
// a function invoked from inside a span carries its own queue calls and
// unresolved calls regardless of whether IT also takes a lock.
func analyzePackage(funcs map[string]*ast.FuncDecl) packageAnalysis {
	direct := map[string]bool{}
	edges := map[string][]string{}
	directUnresolved := map[string][]string{}
	safeFieldsUsed := map[string]bool{}

	for key, fn := range funcs {
		recv := receiverVar(fn)
		recvType := receiverType(fn)
		fvParams := funcValueParamNames(fn)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			kind, ident := classifyCall(call, recv, recvType, fvParams)
			switch kind {
			case callQueue:
				direct[key] = true
			case callMaybeLocal:
				edges[key] = append(edges[key], ident)
			case callUnresolved:
				directUnresolved[key] = append(directUnresolved[key], ident)
			case callIgnored:
				if ident != "" {
					safeFieldsUsed[ident] = true
				}
			}
			return true
		})
	}

	reaches := map[string]bool{}
	maps.Copy(reaches, direct)
	unresolvedReach := map[string][]string{}
	for k, v := range directUnresolved {
		unresolvedReach[k] = appendMissing(nil, v)
	}
	for changed := true; changed; {
		changed = false
		for key, callees := range edges {
			for _, c := range callees {
				if reaches[c] && !reaches[key] {
					reaches[key] = true
					changed = true
				}
				if len(unresolvedReach[c]) > 0 {
					before := len(unresolvedReach[key])
					unresolvedReach[key] = appendMissing(unresolvedReach[key], unresolvedReach[c])
					if len(unresolvedReach[key]) != before {
						changed = true
					}
				}
			}
		}
	}
	return packageAnalysis{reaches: reaches, unresolvedReach: unresolvedReach, safeFieldsUsed: safeFieldsUsed}
}

// appendMissing appends every element of src not already in dst, preserving
// dst's existing order, and returns the result.
func appendMissing(dst, src []string) []string {
	have := make(map[string]bool, len(dst))
	for _, s := range dst {
		have[s] = true
	}
	for _, s := range src {
		if !have[s] {
			dst = append(dst, s)
			have[s] = true
		}
	}
	return dst
}

// checkUnresolved is callUnresolved's handling, shared by a call found
// directly inside a span and one reached transitively through a resolved
// local helper (via, respectively): allowKey names the exact shape that must
// appear in queueCallAllow, with the describing text appended to the
// violation it reports when that entry is missing.
func checkUnresolved(key, ident, via, pos string, allow map[string]string, seenAllow map[string]bool) (violation string) {
	allowKey := key + ": " + ident
	describe := fmt.Sprintf("calls %s (an interface field or func value; this test cannot verify it avoids sched.Queue)", ident)
	if via != "" {
		allowKey = key + ": " + via + " -> " + ident
		describe = fmt.Sprintf("calls %s, which makes an unresolved call %s (an interface field or func value; this test cannot verify it avoids sched.Queue)", via, ident)
	}
	if reason, ok := allow[allowKey]; ok {
		if strings.TrimSpace(reason) != "" {
			seenAllow[allowKey] = true
		}
		return ""
	}
	return fmt.Sprintf(
		"%s: %s while holding d.mu, at %s.\n"+
			"    Move the call out of the span, or add %q to queueCallAllow with the reason it cannot reach sched.Queue.",
		key, describe, pos, allowKey)
}

// scanFuncForLockedQueueCalls walks fn in execution order, tracking whether
// d.mu is held, and reports every call found while it is that either reaches
// sched.Queue (directly, or through a resolved local function that itself
// reaches it) or cannot be resolved and is not in queueCallAllow — including
// a call to a resolved local function that itself makes an unresolved call,
// via unresolvedReach, so a one-line helper cannot hide one. seenAllow
// records every allow-list key an unresolved call actually matched, so the
// caller can also fail a stale entry.
//
// Held-span tracking is a flat toggle over the single ast.Inspect pre-order
// walk, exactly like analyzeLockNesting's inMu/inStoreMu
// (lock_enumeration_test.go): Lock sets it, a non-deferred Unlock clears it,
// a deferred Unlock leaves it set for the remainder of the walk. This is a
// known-accepted simplification shared with that existing gate rather than a
// full control-flow analysis — see its own comment for the shape it does not
// perfectly model (an early Unlock in a branch that does not return would
// understate a later span). Every held span in this package today is either
// a straight Lock...Unlock run or an `if cond { Unlock(); return }` guard
// ahead of a final Unlock, both of which this walk tracks correctly; a
// `switch` of the same guard shape (Add's stopped/restoring check) also
// tracks correctly, because the toggle is set only by a matched Unlock, never
// by a branch merely being visited.
func scanFuncForLockedQueueCalls(key string, fn *ast.FuncDecl, fset *token.FileSet, analysis packageAnalysis, allow map[string]string, seenAllow map[string]bool) (violations []string, sawLock bool) {
	recv := receiverVar(fn)
	recvType := receiverType(fn)
	fvParams := funcValueParamNames(fn)
	deferred := deferredCalls(fn.Body)

	inMu := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if field, verb, isLock := muField(call); isLock {
			if field != "mu" {
				return true // storeMu, or any other lock: not this gate's concern
			}
			sawLock = true
			switch verb {
			case "Lock", "RLock":
				inMu = true
			case "Unlock", "RUnlock":
				if !deferred[call] {
					inMu = false
				}
			}
			return true
		}
		if !inMu {
			return true
		}
		kind, ident := classifyCall(call, recv, recvType, fvParams)
		pos := fset.Position(call.Pos()).String()
		switch kind {
		case callQueue:
			violations = append(violations, fmt.Sprintf("%s: calls %s while holding d.mu, at %s", key, ident, pos))
		case callMaybeLocal:
			if analysis.reaches[ident] {
				violations = append(violations, fmt.Sprintf("%s: calls %s, which reaches sched.Queue, while holding d.mu, at %s", key, ident, pos))
			}
			for _, u := range analysis.unresolvedReach[ident] {
				if v := checkUnresolved(key, u, ident, pos, allow, seenAllow); v != "" {
					violations = append(violations, v)
				}
			}
		case callUnresolved:
			if v := checkUnresolved(key, ident, "", pos, allow, seenAllow); v != "" {
				violations = append(violations, v)
			}
		}
		return true
	})
	return violations, sawLock
}

// TestNoCallIntoQueueUnderDispatcherLock enforces D-B9 (tick.go,
// registry.go): nothing in this package may hold d.mu across a call into
// sched.Queue. It parses every non-test source in the package, computes which
// functions reach sched.Queue and which make or inherit an unresolved call
// (analyzePackage), and then walks every function looking for a Queue call —
// direct, through a resolved local helper, or through something it cannot
// resolve and that queueCallAllow has not excused — inside a d.mu-held span.
func TestNoCallIntoQueueUnderDispatcherLock(t *testing.T) {
	t.Parallel()

	fset, funcs := parseDispatchSources(t)
	analysis := analyzePackage(funcs)

	seenAllow := map[string]bool{}
	var violations []string
	var sawAnyLock bool

	keys := make([]string, 0, len(funcs))
	for key := range funcs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		vs, sawLock := scanFuncForLockedQueueCalls(key, funcs[key], fset, analysis, queueCallAllow, seenAllow)
		violations = append(violations, vs...)
		sawAnyLock = sawAnyLock || sawLock
	}

	if !sawAnyLock {
		t.Fatal("found no d.mu.Lock call anywhere in the package; the AST walk no longer matches the code's shape and this test would pass vacuously")
	}

	for _, v := range violations {
		t.Error(v)
	}

	// A stale entry re-permits the next call that happens to match its text,
	// the same risk manifestGateExempt's own trailing loop guards against.
	for key, reason := range queueCallAllow {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("queueCallAllow[%q] has an empty reason; every entry must justify why the call cannot reach sched.Queue", key)
		}
		if !seenAllow[key] {
			t.Errorf("queueCallAllow lists %q, but no d.mu span calls it anymore; remove the entry", key)
		}
	}

	// Mirrors the queueCallAllow staleness check above: a field declared
	// safe that no call anywhere in the package actually exercises this way
	// is an unverifiable claim, not a verified one.
	for field, reason := range dispatcherSafeFields {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("dispatcherSafeFields[%q] has an empty reason; every entry must justify why the field cannot reach sched.Queue", field)
		}
		if !analysis.safeFieldsUsed[field] {
			t.Errorf("dispatcherSafeFields lists %q, but no d.<field>.<method>(...) call anywhere in the package matches it any more; remove the entry", field)
		}
	}
}

// TestLockSpanGateClassifiesKnownShapes pins the walker's behavior on
// synthetic sources covering the shapes TestNoCallIntoQueueUnderDispatcherLock
// depends on: a direct Queue call under lock, a call to a local helper that
// itself reaches the Queue, a call to a local helper that does NOT, a call
// through an unresolved (interface-shaped) field, a call through a field
// dispatcherSafeFields declares safe, a call through a field NOTHING declares
// safe (the catch-all), and a call to a local helper that itself makes an
// unresolved call (transitive propagation) — most under a deferred unlock, so
// the probe also pins that a deferred Unlock holds the span to the end of the
// function, plus one probe with a non-deferred Unlock pinning that release.
func TestLockSpanGateClassifiesKnownShapes(t *testing.T) {
	t.Parallel()

	const probeSrc = `package dispatch

func (d *Dispatcher) probeDirectQueueUnderLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.q.Advance(nil)
}

func (d *Dispatcher) probeHelperUnderLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probeReachingHelper()
}

func (d *Dispatcher) probeReachingHelper() {
	d.q.Render(nil)
}

func (d *Dispatcher) probeSafeUnderLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probeSafeHelper()
}

func (d *Dispatcher) probeSafeHelper() {
}

func (d *Dispatcher) probeUnresolvedUnderLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runner.Run(nil, "", 0)
}

func (d *Dispatcher) probeSafeFieldUnderLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log.Error("x")
}

func (d *Dispatcher) probeUnknownFieldUnderLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.somethingNew.Frob()
}

func (d *Dispatcher) probeUnresolvedHelper() {
	d.runner.Run(nil, "", 0)
}

func (d *Dispatcher) probeCallsUnresolvedHelperUnderLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probeUnresolvedHelper()
}

func (d *Dispatcher) probeNonDeferredUnlock() {
	d.mu.Lock()
	d.q.Advance(nil)
	d.mu.Unlock()
	d.q.Render(nil)
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", probeSrc, 0)
	if err != nil {
		t.Fatalf("parse probeSrc: %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		funcs[funcKey(fn)] = fn
	}
	analysis := analyzePackage(funcs)

	if !analysis.reaches["Dispatcher.probeReachingHelper"] {
		t.Error("analyzePackage did not mark probeReachingHelper as reaching sched.Queue")
	}
	if !analysis.reaches["Dispatcher.probeDirectQueueUnderLock"] {
		t.Error("analyzePackage did not mark probeDirectQueueUnderLock (direct d.q call) as reaching sched.Queue")
	}
	if analysis.reaches["Dispatcher.probeSafeHelper"] {
		t.Error("analyzePackage wrongly marked probeSafeHelper as reaching sched.Queue")
	}
	if got := analysis.unresolvedReach["Dispatcher.probeUnresolvedHelper"]; len(got) != 1 || got[0] != "d.runner.Run(...)" {
		t.Errorf("analyzePackage.unresolvedReach[probeUnresolvedHelper] = %v, want [d.runner.Run(...)]", got)
	}
	if got := analysis.unresolvedReach["Dispatcher.probeCallsUnresolvedHelperUnderLock"]; len(got) != 1 || got[0] != "d.runner.Run(...)" {
		t.Errorf("analyzePackage.unresolvedReach[probeCallsUnresolvedHelperUnderLock] (propagated through probeUnresolvedHelper) = %v, want [d.runner.Run(...)]", got)
	}
	if !analysis.safeFieldsUsed["log"] {
		t.Error("analyzePackage did not record probeSafeFieldUnderLock's d.log.Error(...) call against dispatcherSafeFields[\"log\"]")
	}

	noAllow := map[string]string{}
	cases := []struct {
		key        string
		wantViol   bool
		wantUnres  bool
		wantSawLok bool
	}{
		{"Dispatcher.probeDirectQueueUnderLock", true, false, true},
		{"Dispatcher.probeHelperUnderLock", true, false, true},
		{"Dispatcher.probeSafeUnderLock", false, false, true},
		{"Dispatcher.probeUnresolvedUnderLock", true, true, true},
		{"Dispatcher.probeSafeFieldUnderLock", false, false, true},
		{"Dispatcher.probeUnknownFieldUnderLock", true, true, true},
		{"Dispatcher.probeCallsUnresolvedHelperUnderLock", true, true, true},
	}
	for _, c := range cases {
		seen := map[string]bool{}
		vs, sawLock := scanFuncForLockedQueueCalls(c.key, funcs[c.key], fset, analysis, noAllow, seen)
		if sawLock != c.wantSawLok {
			t.Errorf("%s: sawLock = %v, want %v", c.key, sawLock, c.wantSawLok)
		}
		if got := len(vs) > 0; got != c.wantViol {
			t.Errorf("%s: violations = %v, want non-empty=%v", c.key, vs, c.wantViol)
		}
		gotUnres := false
		for _, v := range vs {
			if strings.Contains(v, "queueCallAllow") {
				gotUnres = true
			}
		}
		if gotUnres != c.wantUnres {
			t.Errorf("%s: violations mention queueCallAllow = %v, want %v (violations: %v)", c.key, gotUnres, c.wantUnres, vs)
		}
	}

	// A non-deferred Unlock releases the span: a Queue call before it is
	// flagged, and the identical call after it is not.
	seen := map[string]bool{}
	vs, _ := scanFuncForLockedQueueCalls("Dispatcher.probeNonDeferredUnlock", funcs["Dispatcher.probeNonDeferredUnlock"], fset, analysis, noAllow, seen)
	if len(vs) != 1 {
		t.Fatalf("probeNonDeferredUnlock: violations = %v, want exactly 1", vs)
	}
	if !strings.Contains(vs[0], "Advance") {
		t.Errorf("probeNonDeferredUnlock: violation %q does not name the pre-Unlock Advance call", vs[0])
	}
	if strings.Contains(vs[0], "Render") {
		t.Errorf("probeNonDeferredUnlock: violation %q wrongly names the post-Unlock Render call", vs[0])
	}

	// probeUnresolvedUnderLock's call is excused once allow-listed, and the
	// allow key is then marked seen.
	allowKey := "Dispatcher.probeUnresolvedUnderLock: d.runner.Run(...)"
	allowed := map[string]string{allowKey: "test probe: Runner is an interface stub, not a reason to leave this unresolved in production"}
	seenAllowed := map[string]bool{}
	vs, _ = scanFuncForLockedQueueCalls("Dispatcher.probeUnresolvedUnderLock", funcs["Dispatcher.probeUnresolvedUnderLock"], fset, analysis, allowed, seenAllowed)
	if len(vs) != 0 {
		t.Errorf("probeUnresolvedUnderLock with an allow-list entry: violations = %v, want none", vs)
	}
	if !seenAllowed[allowKey] {
		t.Errorf("probeUnresolvedUnderLock's call did not mark %q seen", allowKey)
	}

	// The transitive case (probeCallsUnresolvedHelperUnderLock -> probeUnresolvedHelper
	// -> d.runner.Run) is excused by an allow-list entry naming the whole chain.
	chainKey := "Dispatcher.probeCallsUnresolvedHelperUnderLock: Dispatcher.probeUnresolvedHelper -> d.runner.Run(...)"
	chainAllowed := map[string]string{chainKey: "test probe: pins that an unresolved call is excused via the helper it is reached through"}
	seenChain := map[string]bool{}
	vs, _ = scanFuncForLockedQueueCalls("Dispatcher.probeCallsUnresolvedHelperUnderLock", funcs["Dispatcher.probeCallsUnresolvedHelperUnderLock"], fset, analysis, chainAllowed, seenChain)
	if len(vs) != 0 {
		t.Errorf("probeCallsUnresolvedHelperUnderLock with its chain allow-listed: violations = %v, want none", vs)
	}
	if !seenChain[chainKey] {
		t.Errorf("probeCallsUnresolvedHelperUnderLock's transitive call did not mark %q seen", chainKey)
	}
}
