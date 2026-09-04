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

	found, err := enumerateClosureLockMethods(filepath.Join(repoRoot, "internal"))
	if err != nil {
		t.Fatalf("scan error: %v", err)
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

// enumerateClosureLockMethods inspects all .go files under root and finds
// methods that take a func parameter (directly or via named package-level func type)
// and acquire a lock (Lock/RLock).
func enumerateClosureLockMethods(root string) (map[string]bool, error) {
	methods := make(map[string]*methodInfo)
	packageFuncTypes := make(map[string]map[string]bool) // dir -> typeName -> true
	filesByDir := make(map[string][]string)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		filesByDir[dir] = append(filesByDir[dir], path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	parsedFiles := make(map[string]*ast.File)
	for dir, files := range filesByDir {
		pkgTypes := make(map[string]bool)
		for _, path := range files {
			parsed, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil, err
			}
			parsedFiles[path] = parsed
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
						pkgTypes[ts.Name.Name] = true
					}
				}
			}
		}
		packageFuncTypes[dir] = pkgTypes
	}

	for dir, files := range filesByDir {
		pkgTypes := packageFuncTypes[dir]
		for _, path := range files {
			parsed := parsedFiles[path]
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
					continue
				}
				takesFunc := false
				if fn.Type.Params != nil {
					for _, param := range fn.Type.Params.List {
						if _, isFunc := param.Type.(*ast.FuncType); isFunc {
							takesFunc = true
							break
						}
						if ident, ok := param.Type.(*ast.Ident); ok && pkgTypes[ident.Name] {
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
		}
	}

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
	return found, nil
}

func TestEnumerateClosureLockMethods_SiblingFileTypeResolution(t *testing.T) {
	tmpDir := t.TempDir()
	typesFile := filepath.Join(tmpDir, "types.go")
	methodFile := filepath.Join(tmpDir, "methods.go")

	typesSrc := `package sample
type CallbackFunc func(id string) error
`
	methodSrc := `package sample
import "sync"

type Controller struct {
	mu sync.Mutex
}

func (c *Controller) ExecuteCallback(cb CallbackFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cb("test")
}
`
	if err := os.WriteFile(typesFile, []byte(typesSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(methodFile, []byte(methodSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	found, err := enumerateClosureLockMethods(tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found["ExecuteCallback"] {
		t.Errorf("expected ExecuteCallback to be identified via named func type CallbackFunc declared in sibling file, got: %v", found)
	}
}
