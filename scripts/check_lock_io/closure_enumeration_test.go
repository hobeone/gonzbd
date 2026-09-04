package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type methodInfo struct {
	name      string
	callsLock bool
	callsRecv map[string]bool
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
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
				continue
			}
			// Check if method takes a func argument:
			takesFunc := false
			if fn.Type.Params != nil {
				for _, param := range fn.Type.Params.List {
					if _, isFunc := param.Type.(*ast.FuncType); isFunc {
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

	var foundList []string
	for k := range found {
		foundList = append(foundList, k)
	}
	slices.Sort(foundList)

	var registeredList []string
	for k := range closureLockMethods {
		registeredList = append(registeredList, k)
	}
	slices.Sort(registeredList)

	if !slices.Equal(foundList, registeredList) {
		t.Errorf("closureLockMethods does not match AST enumeration:\nfound in codebase:      %v\nregistered in main.go: %v", foundList, registeredList)
	}
}
