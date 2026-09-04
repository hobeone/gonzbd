package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type methodInfo struct {
	name      string
	callsLock bool
	callsRecv map[string]bool
}

// closureMethodExclusions are methods taking a closure argument that acquire a lock
// but deliberately do not hold the lock across the closure callback (or are
// otherwise excluded from closureLockMethods), with the mandatory reason.
var closureMethodExclusions = map[string]string{
	// method_name: "mandatory reason why lock is not held across callback"
}

// TestClosureLockMethods_MatchEnumeration asserts that closureLockMethods in
// main.go matches every method in the codebase that takes a closure (func
// argument) while acquiring a lock (.Lock() or .RLock()), either directly
// or via forwarding to an inner locking helper method.
func TestClosureLockMethods_MatchEnumeration(t *testing.T) {
	repoRoot := findRepoRoot(".")
	if repoRoot == "" {
		t.Fatal("could not find repo root")
	}

	methods := make(map[string]*methodInfo)

	err := filepath.Walk(filepath.Join(repoRoot, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		// Index package-level named func types (e.g. type FileIdxFunc func(...) ...)
		packageFuncTypes := make(map[string]bool)
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil {
					continue
				}
				if _, isFunc := ts.Type.(*ast.FuncType); isFunc {
					packageFuncTypes[ts.Name.Name] = true
				}
			}
		}

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
				continue
			}
			// Check if method takes a func argument (directly or via named func type):
			takesFunc := false
			if fn.Type.Params != nil {
				for _, param := range fn.Type.Params.List {
					if _, isFunc := param.Type.(*ast.FuncType); isFunc {
						takesFunc = true
						break
					}
					if ident, ok := param.Type.(*ast.Ident); ok && packageFuncTypes[ident.Name] {
						takesFunc = true
						break
					}
				}
			}
			if !takesFunc {
				continue
			}

			mi := &methodInfo{
				name:      fn.Name.Name,
				callsRecv: make(map[string]bool),
			}

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
					mi.callsLock = true
				} else {
					mi.callsRecv[sel.Sel.Name] = true
				}
				return true
			})
			methods[fn.Name.Name] = mi
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}

	// Resolve locking methods (direct or 1-level helper call)
	found := make(map[string]bool)
	for name, mi := range methods {
		if mi.callsLock {
			found[name] = true
			continue
		}
		for called := range mi.callsRecv {
			if target, ok := methods[called]; ok && target.callsLock {
				found[name] = true
				break
			}
		}
	}

	for name := range found {
		if closureLockMethods[name] {
			continue
		}
		if reason, ok := closureMethodExclusions[name]; ok {
			if reason == "" {
				t.Errorf("closureMethodExclusions names %q with an empty reason; a mandatory reason is required", name)
			} else {
				t.Logf("closure method %s excluded from closureLockMethods: %s", name, reason)
			}
			continue
		}
		t.Errorf("method %s takes a func and acquires a lock, but is not registered in closureLockMethods or closureMethodExclusions; register it in main.go or exclude it with a reason", name)
	}

	for name := range closureLockMethods {
		if found[name] {
			continue
		}
		t.Errorf("closureLockMethods registers %q, which was not found by AST enumeration; the detector will match a non-existent method", name)
	}

	for name, reason := range closureMethodExclusions {
		if reason == "" {
			t.Errorf("closureMethodExclusions names %q with an empty reason; a mandatory reason is required", name)
		}
		if !found[name] {
			t.Errorf("closureMethodExclusions names %q, which was not found by AST enumeration; drop the stale exclusion", name)
		}
	}
}
