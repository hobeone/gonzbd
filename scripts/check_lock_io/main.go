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
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"os/exec" //nolint:gosec // dev tool, fixed command args
	"path/filepath"
	"slices"
	"sort"
	"strings"
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

// closureLockMethods is the allowlist of methods that hold a lock for the
// duration of a func-literal argument. Confirmed by exhaustive repo-wide
// grep to be the only three such methods as of this writing:
// config.Config.WithRead, config.Config.With, and
// queue.Queue.ForEachUnfinishedArticle. Adding a new lock-wrapping closure
// method anywhere in the repo requires updating this list (see
// docs/go-standards.md).
var closureLockMethods = map[string]bool{
	"WithRead":                 true,
	"With":                     true,
	"ForEachUnfinishedArticle": true,
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

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		fmt.Printf("  ✗ %s:%d: %s\n", f.file, f.line, f.desc)
	}
	fmt.Printf("\nStatus: Found %d mutex-held-during-I/O violation(s).\n", len(findings))
	return 1
}

// gatherTargetFiles returns the non-test .go files to check: every file
// under internal/ and cmd/ in --all mode, or just the files touched in the
// current diff (origin/main...HEAD, falling back to HEAD~1) otherwise.
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

	cmd := exec.Command("git", "diff", "--name-only", "origin/main...HEAD")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("git", "diff", "--name-only", "HEAD~1")
		stdout.Reset()
		cmd.Stdout = &stdout
		_ = cmd.Run()
	}

	var files []string
	for f := range strings.SplitSeq(stdout.String(), "\n") {
		f = strings.TrimSpace(f)
		if f == "" || !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
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

	var findings []finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		findings = append(findings, checkFuncBody(fset, path, fn.Body, commentsByLine)...)
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
func checkFuncBody(fset *token.FileSet, path string, body *ast.BlockStmt, commentsByLine map[int]string) []finding {
	deferredFrom := collectDeferredLocks(body)

	w := &walker{
		fset:           fset,
		path:           path,
		commentsByLine: commentsByLine,
		deferredFrom:   deferredFrom,
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
	findings       []finding
}

// walkClosures finds every closure-wrapper call (WithRead/With/
// ForEachUnfinishedArticle, or any future addition to closureLockMethods)
// anywhere in body and treats its sole func-literal argument's entire body
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
		if len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok {
			return true
		}
		desc := fmt.Sprintf("I/O call inside %s(...) closure (holds a lock for its entire body)", sel.Sel.Name)
		w.scanForIO(lit.Body, desc)
		return true
	})
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
	sort.Strings(active)
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
		if ioDesc, ok := isIOCall(call); ok {
			w.report(call.Pos(), desc+" ("+ioDesc+")")
		}
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
		if ioDesc, ok := isIOCall(call); ok {
			w.report(call.Pos(), desc+" ("+ioDesc+")")
		}
		return true
	})
}

// isIOCall reports whether call is a logging, disk-I/O, or network call,
// along with a short description for the finding message.
func isIOCall(call *ast.CallExpr) (desc string, ok bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}

	// A closure-wrapper call itself (e.g. cfg.WithRead(func...)) is not an
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
func (w *walker) report(pos token.Pos, desc string) {
	line := w.fset.Position(pos).Line
	if strings.Contains(w.commentsByLine[line], "lockio:") {
		return
	}
	w.findings = append(w.findings, finding{file: w.path, line: line, desc: desc})
}
