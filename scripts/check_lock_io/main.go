// Command check_lock_io reports calls to logging, file-I/O, or network
// functions made while a mutex or config lock-wrapper closure is held — the
// "never hold a mutex during disk I/O or network calls" rule in
// docs/go-standards.md. It is a heuristic, not a soundness guarantee:
// detection is based on method-name and argument-shape matching against
// go/ast, not on type information (no go/types checking, no go/packages —
// this tool deliberately stays stdlib-only, matching check_coverage and
// check_test_alignment). A finding can be suppressed with a same-line
// trailing `//lockio: <reason>` comment, mirroring the //nocover: convention.
//
// Scope limitation: each check only analyzes one top-level function body
// (*ast.FuncDecl) at a time. A closure (goroutine literal, IIFE, callback)
// is its own separate scope for lock-state purposes and is deliberately not
// descended into by the manual-lock walker or the deferred-lock collector —
// so a Lock()/Unlock() pair used entirely within one closure's own body,
// with an I/O call also entirely within that same closure, is not checked.
package main

import (
	"cmp"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/hobeone/gonzbd/scripts/gitscope"
)

// loggerMethods are *slog.Logger-shaped method names. Matched by name alone
// (no type information), so a false positive is possible if some other type
// happens to define a method with the same name and is called directly
// (not via a closure-wrapper) inside a locked span — accepted risk for a
// heuristic dev tool; suppress with //lockio: if this ever fires wrongly.
var loggerMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true,
}

// osIOFuncs are package-qualified os.* file operations, derived from actual
// usage in this repo (internal/ and cmd/), not guessed.
var osIOFuncs = map[string]bool{
	"Open": true, "Create": true, "CreateTemp": true, "OpenFile": true,
	"WriteFile": true, "ReadFile": true, "ReadDir": true, "Readlink": true,
	"Remove": true, "RemoveAll": true, "Rename": true,
	"Mkdir": true, "MkdirAll": true, "MkdirTemp": true, "Stat": true,
}

// netIOFuncs and httpIOFuncs are package-qualified network calls. nntp is
// this project's own NNTP client package (internal/nntp) — its Dial is a
// real network call, found by direct inspection to be missed by a
// stdlib-only net/http check.
var netIOFuncs = map[string]bool{
	"Dial": true, "DialContext": true, "DialTimeout": true, "Listen": true,
}

var nntpIOFuncs = map[string]bool{
	"Dial": true,
}

var httpIOFuncs = map[string]bool{
	"Get": true, "Post": true, "PostForm": true, "Head": true,
}

// sqlReceivers are the identifiers this repo binds to a *sql.DB or *sql.Tx.
// Confirmed by inspection rather than guessed: every database call site in
// internal/ reaches the handle as `db.X(...)`, `tx.X(...)`, `s.db.X(...)` or
// `r.db.X(...)`. Any method on one of these is a database round trip, so the
// method name is not restricted — Query, Exec, Begin, Commit and Close are
// all I/O.
//
// This is the gap that let a SQLite transaction run under the global queue
// mutex go unreported (#227): the tool had no notion of database calls at all.
var sqlReceivers = map[string]bool{
	"db": true, "tx": true,
}

// storeMethods are persistence operations on a `store` receiver. Unlike
// db/tx, `store` is not unambiguous — internal/dirscanner uses it for an
// in-memory path map — so matching is restricted to a method set rather than
// any call. The names come from the queue.Store interface
// (internal/queue/store.go), which is backed by SQLite, plus dirscanner's
// storeMethods lists the method names of types performing SQLite store I/O.
//
// Delete is deliberately excluded: it is an operation on dirscanner's in-memory store
// and too generic to attribute.
//
// A name here that does not match a real method disables its check
// silently — the detector just never matches, and the gate still reports
// success. TestStoreMethodsMatchStoreInterface pins this table against
// dispatch.Store in both directions; record any new omission in that test's
// storeMethodExclusions with a reason rather than leaving it unlisted.
var storeMethods = map[string]bool{
	"Load": true,
	"Save": true,
}

// lockedSuffix is the naming convention this repo uses for "the caller must
// already hold the lock". It is what makes one level of call-graph
// resolution cheap and high-precision: a call to a *Locked function from
// inside a locked span is, by construction, still inside that span, so its
// body is worth scanning even though it lives in a separate FuncDecl.
const lockedSuffix = "Locked"

// closureLockMethods is the allowlist of methods that hold a lock for the
// duration of a func-literal argument. Confirmed by AST scan across the entire
// repository (enforced by TestClosureLockMethods_MatchEnumeration in
// closure_enumeration_test.go).
var closureLockMethods = map[string]bool{
	"With":                     true,
	"ForEachUnfinishedArticle": true,
	"withOpenAttempt":          true,
	"withOpenAttemptLease":     true,
	"Pop":                      true,
	"tryPop":                   true,
	"withLock":                 true,
}

// jobClosureMethods specifies the closureLockMethods that execute under a
// Job lock (such as j.contentMu or j.mu). For these methods, calling
// Job methods that acquire locks inside the closure constitutes a lock-inversion hazard.
var jobClosureMethods = map[string]bool{
	"ForEachUnfinishedArticle": true,
	"withOpenAttempt":          true,
	"withOpenAttemptLease":     true,
}

type finding struct {
	file string
	line int
	desc string
}

func main() {
	os.Exit(run())
}

func run() int {
	all := flag.Bool("all", false, "scan every non-test .go file under internal/ and cmd/ instead of the git diff")
	flag.Parse()

	targetFiles, err := gatherTargetFiles(*all)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error gathering target files: %v\n", err)
		return 1
	}
	if len(targetFiles) == 0 {
		fmt.Println("No source Go files found to check.")
		return 0
	}

	var findings []finding
	for _, path := range targetFiles {
		fs, err := checkFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error checking %s: %v\n", path, err)
			continue
		}
		findings = append(findings, fs...)
	}

	if len(findings) == 0 {
		fmt.Println("Status: No mutex-held-during-I/O violations found.")
		return 0
	}

	slices.SortFunc(findings, func(a, b finding) int {
		if a.file != b.file {
			return cmp.Compare(a.file, b.file)
		}
		return cmp.Compare(a.line, b.line)
	})
	for _, f := range findings {
		fmt.Printf("  ✗ %s:%d: %s\n", f.file, f.line, f.desc)
	}
	fmt.Printf("\nStatus: Found %d mutex-held-during-I/O violation(s).\n", len(findings))
	return 1
}

// gatherTargetFiles returns the non-test .go files to check: every file
// under internal/ and cmd/ in --all mode, or just the files touched in the
// current change scope otherwise -- the committed range plus uncommitted
// work, so the gate gives signal before a commit (see scripts/gitscope).
func gatherTargetFiles(all bool) ([]string, error) {
	if all {
		var files []string
		for _, root := range []string{"internal", "cmd"} {
			err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		return files, nil
	}

	changed, err := gitscope.Files()
	if err != nil {
		return nil, err
	}

	var files []string
	for _, f := range changed {
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		if !strings.HasPrefix(f, "internal/") && !strings.HasPrefix(f, "cmd/") {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	return files, nil
}

// packageFuncCache memoises the per-package map of function and method
// declarations so a package with many changed files is parsed once rather
// than once per file.
var packageFuncCache = map[string]map[string]*ast.FuncDecl{}

// packageFuncsInPackage returns every function and method declared in dir,
// keyed by name.
func packageFuncsInPackage(dir string) map[string]*ast.FuncDecl {
	if cached, ok := packageFuncCache[dir]; ok {
		return cached
	}
	funcs := map[string]*ast.FuncDecl{}
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err == nil {
		for _, f := range entries {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, perr := parser.ParseFile(token.NewFileSet(), f, nil, 0)
			if perr != nil {
				continue
			}
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				funcs[fn.Name.Name] = fn
			}
		}
	}
	packageFuncCache[dir] = funcs
	return funcs
}

// lockedFuncsInPackage returns every *Locked function and method declared in
// dir, keyed by name.
func lockedFuncsInPackage(dir string) map[string]*ast.FuncDecl {
	all := packageFuncsInPackage(dir)
	locked := make(map[string]*ast.FuncDecl)
	for k, v := range all {
		if strings.HasSuffix(k, lockedSuffix) {
			locked[k] = v
		}
	}
	return locked
}

var (
	jobLockMethodsOnce sync.Once
	jobLockMethods     map[string]bool
)

// findRepoRoot finds the repository root directory by walking up from the current
// working directory or an optional start path until go.mod and internal/job are found.
func findRepoRoot(start string) string {
	check := func(dir string) string {
		for {
			if _, err := os.Stat(filepath.Join(dir, "internal", "job")); err == nil {
				if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
					return dir
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		return ""
	}

	if start != "" {
		if abs, err := filepath.Abs(start); err == nil {
			if root := check(abs); root != "" {
				return root
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if root := check(cwd); root != "" {
			return root
		}
	}
	return ""
}

// jobMethodsAcquiringLocks returns a set of method names declared on *Job
// in internal/job/*.go that contain a direct call to .Lock() or .RLock().
func jobMethodsAcquiringLocks(startDir string) map[string]bool {
	jobLockMethodsOnce.Do(func() {
		jobLockMethods = make(map[string]bool)
		repoRoot := findRepoRoot(startDir)
		if repoRoot == "" {
			return
		}
		entries, err := filepath.Glob(filepath.Join(repoRoot, "internal", "job", "*.go"))
		if err != nil {
			return
		}
		for _, f := range entries {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
			if err != nil {
				continue
			}
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
					continue
				}
				recvType := fn.Recv.List[0].Type
				star, ok := recvType.(*ast.StarExpr)
				if !ok {
					continue
				}
				ident, ok := star.X.(*ast.Ident)
				if !ok || ident.Name != "Job" {
					continue
				}

				hasLock := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if sel.Sel.Name == "Lock" || sel.Sel.Name == "RLock" {
						hasLock = true
						return false
					}
					return true
				})
				if hasLock {
					jobLockMethods[fn.Name.Name] = true
				}
			}
		}
	})
	return jobLockMethods
}

// checkFile parses one Go source file and returns every mutex-held-during-
// I/O finding in it, filtered by //lockio: suppression comments.
func checkFile(path string) ([]finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	commentsByLine := make(map[int]string)
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			line := fset.Position(c.Pos()).Line
			commentsByLine[line] += " " + c.Text
		}
	}

	// One level of call-graph resolution: a call to a *Locked helper from
	// inside a locked span keeps the lock held for that helper's body too.
	lockedFuncs := lockedFuncsInPackage(filepath.Dir(path))
	packageFuncs := packageFuncsInPackage(filepath.Dir(path))

	var findings []finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		findings = append(findings, checkFuncBody(fset, path, fn.Body, commentsByLine, lockedFuncs, packageFuncs)...)
	}
	return findings, nil
}

// checkFuncBody analyzes one function or method body. It layers three
// detectors: deferred Lock()/Unlock() pairs (function-scoped — a defer
// always fires at the enclosing function's return, regardless of block
// nesting, so these are computed once up front, separately from the
// recursive block walk below); closure-wrapper calls (their entire
// func-literal argument is a locked span); and manual (non-deferred)
// Lock()/Unlock() pairs, found via a block-scoped walk that inherits lock
// state into nested blocks as a copy — a branch's own unlock-then-return
// does not leak back to affect sibling statements after the branch, which
// is what lets an early "unlock, log, return" pattern (the repo's own
// correct idiom) pass cleanly instead of producing a false positive.
func checkFuncBody(fset *token.FileSet, path string, body *ast.BlockStmt, commentsByLine map[int]string, lockedFuncs map[string]*ast.FuncDecl, packageFuncs map[string]*ast.FuncDecl) []finding {
	deferredFrom := collectDeferredLocks(body)

	w := &walker{
		fset:           fset,
		path:           path,
		commentsByLine: commentsByLine,
		deferredFrom:   deferredFrom,
		lockedFuncs:    lockedFuncs,
		packageFuncs:   packageFuncs,
	}
	w.walkClosures(body)
	w.walkBlock(body.List, map[string]bool{})
	return w.findings
}

// collectDeferredLocks scans the whole function body (regardless of block
// nesting) for a Lock()/RLock() statement immediately followed, in the same
// statement list, by a DeferStmt calling the matching Unlock()/RUnlock() on
// the same receiver. Each such receiver is considered locked from that
// Lock() call's position to the end of the function, since Go's defer
// always fires at return.
func collectDeferredLocks(body *ast.BlockStmt) map[string]token.Pos {
	deferredFrom := make(map[string]token.Pos)
	ast.Inspect(body, func(n ast.Node) bool {
		// A closure (goroutine literal, IIFE, callback) is its own scope: a
		// Lock()+defer Unlock() pair inside one only holds for that
		// closure's own execution, which may run concurrently and has no
		// textual-position relationship to the enclosing function's other
		// statements. Without this boundary, a lock deferred inside a `go
		// func(){...}()` was incorrectly treated as covering unrelated code
		// later in the outer function too (a false positive).
		if _, isFuncLit := n.(*ast.FuncLit); isFuncLit {
			return false
		}
		list := blockList(n)
		if list == nil {
			return true
		}
		for i, stmt := range list {
			recv, lockKind := matchLockCall(stmt)
			if recv == "" {
				continue
			}
			if i+1 >= len(list) {
				continue
			}
			def, ok := list[i+1].(*ast.DeferStmt)
			if !ok {
				continue
			}
			drecv, unlockKind := matchUnlockCallExpr(def.Call)
			if drecv != recv || !matchingLockKind(lockKind, unlockKind) {
				continue
			}
			if existing, ok := deferredFrom[recv]; !ok || stmt.Pos() < existing {
				deferredFrom[recv] = stmt.Pos()
			}
		}
		return true
	})
	return deferredFrom
}

// blockList returns the statement list of n if n is a node that owns one
// directly (as opposed to via a nested *ast.BlockStmt field, which
// ast.Inspect will visit on its own).
func blockList(n ast.Node) []ast.Stmt {
	switch b := n.(type) {
	case *ast.BlockStmt:
		return b.List
	case *ast.CaseClause:
		// switch/type-switch case bodies are a raw []ast.Stmt, not wrapped
		// in a *ast.BlockStmt — without this case, a Lock()+defer Unlock()
		// pair starting as a case's first statements is invisible here,
		// even though the deferred unlock still only fires at the
		// enclosing function's return, same as anywhere else.
		return b.Body
	case *ast.CommClause:
		// select case bodies have the same raw-[]ast.Stmt shape as CaseClause.
		return b.Body
	}
	return nil
}

// matchLockCall reports the receiver expression string and lock kind
// ("Lock" or "RLock") if stmt is an ExprStmt calling X.Lock() or X.RLock().
func matchLockCall(stmt ast.Stmt) (receiver, kind string) {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return "", ""
	}
	return matchUnlockCallExpr(es.X) // same shape: X.Method() with no args
}

// matchUnlockCallExpr reports the receiver and method name for any
// zero-argument X.Method() call expression — used for both Lock/RLock and
// Unlock/RUnlock, which all share this shape.
func matchUnlockCallExpr(expr ast.Expr) (receiver, method string) {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return "", ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	switch sel.Sel.Name {
	case "Lock", "RLock", "Unlock", "RUnlock":
		return types.ExprString(sel.X), sel.Sel.Name
	default:
		return "", ""
	}
}

// matchingLockKind reports whether a Lock/Unlock method pair refers to the
// same lock mode (Lock<->Unlock, RLock<->RUnlock).
func matchingLockKind(lockKind, unlockKind string) bool {
	switch lockKind {
	case "Lock":
		return unlockKind == "Unlock"
	case "RLock":
		return unlockKind == "RUnlock"
	default:
		return false
	}
}

type walker struct {
	fset           *token.FileSet
	path           string
	commentsByLine map[int]string
	deferredFrom   map[string]token.Pos
	// lockedFuncs maps *Locked declarations in this package by name, for the
	// one-level call-graph descent (see scanCall).
	lockedFuncs map[string]*ast.FuncDecl
	// packageFuncs maps all declarations in this package by name, for the
	// one-level call-graph descent in closures (see scanClosureForLocks).
	packageFuncs map[string]*ast.FuncDecl
	findings     []finding
}

// walkClosures finds every closure-wrapper call (With/
// ForEachUnfinishedArticle, or any future addition to closureLockMethods)
// anywhere in body and treats its func-literal argument's entire body
// as a locked span, regardless of which block it's nested in.
func (w *walker) walkClosures(body *ast.BlockStmt) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !closureLockMethods[sel.Sel.Name] {
			return true
		}
		var lit *ast.FuncLit
		for _, arg := range call.Args {
			if f, ok := arg.(*ast.FuncLit); ok {
				lit = f
				break
			}
		}
		if lit == nil {
			return true
		}
		desc := fmt.Sprintf("I/O call inside %s(...) closure (holds a lock for its entire body)", sel.Sel.Name)
		w.scanForIO(lit.Body, desc)
		w.scanClosureForLocks(lit.Body, sel.Sel.Name)
		return true
	})
}

// scanClosureForLocks inspects a closure passed to a closureLockMethod
// (e.g. With, ForEachUnfinishedArticle) for lock acquisitions, preventing
// Class 4 lock-inversion deadlocks:
// 1) Direct mutex acquisitions: any call to Lock() or RLock().
// 2) Method calls on job.Job that acquire locks (e.g. j.Added()).
// 3) One-level call-graph descent into package-level helper functions.
func (w *walker) scanClosureForLocks(body *ast.BlockStmt, closureMethod string) {
	jobLocking := jobMethodsAcquiringLocks(filepath.Dir(w.path))
	isJobClosure := jobClosureMethods[closureMethod]

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		// Descend one level into package-local helper functions (e.g. addedOf(j))
		name := calleeNameExpr(call.Fun)
		if helper, ok := w.packageFuncs[name]; ok && helper.Body != nil {
			ast.Inspect(helper.Body, func(hn ast.Node) bool {
				hcall, ok := hn.(*ast.CallExpr)
				if !ok {
					return true
				}
				if hsel, ok := hcall.Fun.(*ast.SelectorExpr); ok {
					if hsel.Sel.Name == "Lock" || hsel.Sel.Name == "RLock" {
						desc := fmt.Sprintf("lock acquisition inside %s(...) closure (via %s): %s()", closureMethod, name, types.ExprString(hcall.Fun))
						w.report(call.Pos(), call.End(), desc)
						return true
					}
					if isJobClosure && jobLocking[hsel.Sel.Name] {
						desc := fmt.Sprintf("lock acquisition inside %s(...) closure (via %s): %s()", closureMethod, name, types.ExprString(hcall.Fun))
						w.report(call.Pos(), call.End(), desc)
						return true
					}
				}
				return true
			})
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		// 1) Direct mutex acquisitions: call to .Lock() or .RLock()
		if sel.Sel.Name == "Lock" || sel.Sel.Name == "RLock" {
			desc := fmt.Sprintf("lock acquisition inside %s(...) closure: %s()", closureMethod, types.ExprString(call.Fun))
			w.report(call.Pos(), call.End(), desc)
			return true
		}

		// 2) Methods on job.Job that acquire locks (only checked for Job closure methods)
		if isJobClosure && jobLocking[sel.Sel.Name] {
			desc := fmt.Sprintf("lock acquisition inside %s(...) closure: %s()", closureMethod, types.ExprString(call.Fun))
			w.report(call.Pos(), call.End(), desc)
			return true
		}

		return true
	})
}

func calleeNameExpr(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	default:
		return ""
	}
}

// walkBlock recursively walks a statement list, maintaining a local,
// copy-on-recurse map of which receivers are currently locked. A nested
// block (if/for/switch/select body) is walked with a COPY of the current
// locked set: mutations inside the nested block (an unlock followed by an
// early return, the repo's own correct idiom) do not propagate back to
// affect this list's continued iteration after the nested statement.
func (w *walker) walkBlock(list []ast.Stmt, locked map[string]bool) {
	for _, stmt := range list {
		if recv, kind := matchLockCall(stmt); recv != "" && (kind == "Lock" || kind == "RLock") {
			locked[recv] = true
			continue
		}
		if recv, kind := matchLockCall(stmt); recv != "" && (kind == "Unlock" || kind == "RUnlock") {
			locked[recv] = false
			continue
		}

		w.checkStmtForIO(stmt, locked)
		w.recurseNested(stmt, locked)
	}
}

// checkStmtForIO scans a single statement (not descending into nested
// blocks, which walkBlock's recursion handles separately) for I/O calls,
// reporting one for each locked receiver active at this point — including
// receivers locked via a deferred Unlock() anywhere earlier in the
// function, per collectDeferredLocks.
func (w *walker) checkStmtForIO(stmt ast.Stmt, locked map[string]bool) {
	active := activeReceivers(locked, w.deferredFrom, stmt.Pos())
	if len(active) == 0 {
		return
	}
	desc := fmt.Sprintf("I/O call while holding lock(s): %s", strings.Join(active, ", "))
	w.scanStmtShallow(stmt, desc)
}

// activeReceivers returns every receiver locked at pos, combining the
// block-walk's local (manual-unlock) state with the function-global
// deferred-lock map.
func activeReceivers(locked map[string]bool, deferredFrom map[string]token.Pos, pos token.Pos) []string {
	var active []string
	for recv, isLocked := range locked {
		if isLocked {
			active = append(active, recv)
		}
	}
	for recv, from := range deferredFrom {
		if pos > from && !locked[recv] {
			// Not already counted via the manual-unlock branch above
			// (locked[recv] would be false or absent there too, since a
			// deferred receiver is never explicitly re-locked/unlocked in
			// the same block in the patterns this tool targets).
			alreadyListed := slices.Contains(active, recv)
			if !alreadyListed {
				active = append(active, recv)
			}
		}
	}
	slices.Sort(active)
	return active
}

// recurseNested descends into any nested block(s) a statement owns, with a
// COPY of the current locked set.
func (w *walker) recurseNested(stmt ast.Stmt, locked map[string]bool) {
	cp := func() map[string]bool {
		c := make(map[string]bool, len(locked))
		maps.Copy(c, locked)
		return c
	}

	switch s := stmt.(type) {
	case *ast.IfStmt:
		w.walkBlock(s.Body.List, cp())
		if s.Else != nil {
			if elseBlock, ok := s.Else.(*ast.BlockStmt); ok {
				w.walkBlock(elseBlock.List, cp())
			} else {
				w.recurseNested(s.Else, cp())
			}
		}
	case *ast.ForStmt:
		w.walkBlock(s.Body.List, cp())
	case *ast.RangeStmt:
		w.walkBlock(s.Body.List, cp())
	case *ast.SwitchStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok {
				w.walkBlock(cc.Body, cp())
			}
		}
	case *ast.TypeSwitchStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok {
				w.walkBlock(cc.Body, cp())
			}
		}
	case *ast.SelectStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CommClause); ok {
				w.walkBlock(cc.Body, cp())
			}
		}
	case *ast.BlockStmt:
		w.walkBlock(s.List, cp())
	case *ast.LabeledStmt:
		// A labeled statement (e.g. `retry:` before a for/if/switch, used
		// with labeled break/continue) wraps exactly one nested statement in
		// its own Stmt field — recurse into it the same as if the label
		// weren't there, or the label silently makes everything inside it
		// invisible to this walk.
		w.recurseNested(s.Stmt, cp())
	}
}

// scanStmtShallow reports every I/O call found in stmt (descending into
// expressions but not into nested statement blocks — those are handled by
// the caller's own recursion with correctly-scoped lock state) using desc
// as the finding description, honoring //lockio: suppression.
func (w *walker) scanStmtShallow(stmt ast.Stmt, desc string) {
	ast.Inspect(stmt, func(n ast.Node) bool {
		// Don't descend into nested blocks; their own I/O calls are
		// evaluated by the caller against their own (possibly different)
		// lock state.
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.SelectStmt, *ast.BlockStmt, *ast.FuncLit:
			return n == stmt
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		w.scanCall(call, desc)
		return true
	})
}

// scanForIO reports every I/O call anywhere within node (used for
// closure-wrapper bodies, where the entire body is unconditionally locked).
func (w *walker) scanForIO(node ast.Node, desc string) {
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		w.scanCall(call, desc)
		return true
	})
}

// scanCall reports call if it is itself I/O, and otherwise descends one level
// into it when it targets a *Locked helper in the same package.
//
// The descent is what closes the blind spot behind a callee: a lock taken in
// the caller with the I/O one frame down was invisible to a walk that only
// ever looked at a single FuncDecl body. Findings are reported at the *call
// site*, not inside the callee, because that is where the lock is held, where
// the fix belongs, and where a //lockio: marker can be written.
//
// Depth is deliberately one. A *Locked helper calling another one is rare
// here, and unbounded descent would trade the tool's cheap, predictable
// behaviour for recursion cycles and a far larger false-positive surface.
func (w *walker) scanCall(call *ast.CallExpr, desc string) {
	if ioDesc, ok := isIOCall(call); ok {
		w.report(call.Pos(), call.End(), desc+" ("+ioDesc+")")
		return
	}

	name := calleeName(call)
	if !strings.HasSuffix(name, lockedSuffix) {
		return
	}
	callee, ok := w.lockedFuncs[name]
	if !ok || callee.Body == nil {
		return
	}

	ast.Inspect(callee.Body, func(n ast.Node) bool {
		inner, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ioDesc, ok := isIOCall(inner)
		if !ok {
			return true
		}
		// Positions inside the callee belong to a different FileSet (and
		// possibly a different file), so anchor the finding to the call site
		// and name the callee in the description instead.
		w.report(call.Pos(), call.End(),
			fmt.Sprintf("%s (via %s: %s)", desc, name, ioDesc))
		return true
	})
}

// receiverName returns the identifier immediately left of a selector's
// method: "db" for both `db.Query` and `s.db.Query`. Returns "" for shapes
// with no such identifier (an index expression, a call result, and so on).
func receiverName(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

// calleeName returns the called function's own name, ignoring any receiver:
// "fooLocked" for `fooLocked()`, `q.fooLocked()` and `d.tracker.fooLocked()`.
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// isIOCall reports whether call is a logging, disk-I/O, network, or
// persistence call, along with a short description for the finding message.
func isIOCall(call *ast.CallExpr) (desc string, ok bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}

	// A closure-wrapper call itself (e.g. cfg.With(func...)) is not an
	// I/O call — its body is handled separately by walkClosures.
	if closureLockMethods[sel.Sel.Name] && len(call.Args) == 1 {
		if _, isFuncLit := call.Args[0].(*ast.FuncLit); isFuncLit {
			return "", false
		}
	}

	// slog.Logger's methods always take at least one argument (the
	// message); the stdlib error interface's Error() takes none. Requiring
	// >=1 arg disambiguates a logging call from a plain err.Error() call —
	// "Error" is the one name in loggerMethods that collides with a very
	// common unrelated method.
	if loggerMethods[sel.Sel.Name] && len(call.Args) >= 1 {
		return "slog-style logging call: " + sel.Sel.Name + "(...)", true
	}

	// Persistence calls reach their handle either bare (`db.Query(...)`) or
	// through one field hop (`s.db.Query(...)`), so match on the identifier
	// immediately left of the method rather than requiring a bare package
	// ident the way the stdlib cases below do.
	switch recv := receiverName(sel.X); {
	case sqlReceivers[recv]:
		return "database/sql call: " + recv + "." + sel.Sel.Name + "(...)", true
	case recv == "store" && storeMethods[sel.Sel.Name]:
		return "persistence call: store." + sel.Sel.Name + "(...)", true
	}

	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	switch pkgIdent.Name {
	case "os":
		if osIOFuncs[sel.Sel.Name] {
			return "os." + sel.Sel.Name + "(...)", true
		}
	case "net":
		if netIOFuncs[sel.Sel.Name] {
			return "net." + sel.Sel.Name + "(...)", true
		}
	case "nntp":
		if nntpIOFuncs[sel.Sel.Name] {
			return "nntp." + sel.Sel.Name + "(...)", true
		}
	case "http":
		if httpIOFuncs[sel.Sel.Name] {
			return "http." + sel.Sel.Name + "(...)", true
		}
	}
	return "", false
}

// report records a finding at pos, unless the same line carries a
// //lockio: suppression comment.
// report records a finding spanning [start, end), unless any line in that
// range carries a //lockio: suppression comment. Checking the whole span
// (not just the start line) matters for a multi-line call — the natural
// place for a trailing //lockio: comment is after the closing paren, on the
// call's LAST line, not its first.
func (w *walker) report(start, end token.Pos, desc string) {
	startLine := w.fset.Position(start).Line
	endLine := w.fset.Position(end).Line
	for line := startLine; line <= endLine; line++ {
		if strings.Contains(w.commentsByLine[line], "lockio:") {
			return
		}
	}
	w.findings = append(w.findings, finding{file: w.path, line: startLine, desc: desc})
}
