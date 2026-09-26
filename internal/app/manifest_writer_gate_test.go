package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestManifestPath_HasOneProductionCaller pins writeJobManifest as the only
// production function that references manifestPath — a call, or the function
// taken as a value — across this package's non-test files. Reads go through
// openManifestIn and deletes through removeManifestIn, so a second function
// here would be a second writer of the queue manifest.
//
// It sees only writers that go through manifestPath. One that joined
// manifestDir and manifestName itself would not appear.
func TestManifestPath_HasOneProductionCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "manifestPath" {
					callers = append(callers, name+":"+fn.Name.Name)
					return false
				}
				return true
			})
		}
	}
	slices.Sort(callers)
	callers = slices.Compact(callers)

	want := []string{"manifestpath.go:writeJobManifest"}
	if !slices.Equal(callers, want) {
		t.Errorf("functions referencing manifestPath = %v, want %v. Every other site "+
			"that writes a queue manifest must call writeJobManifest, or the two copies "+
			"drift and a retried or added job can reach the queue with a manifest "+
			"hydration cannot read", callers, want)
	}
}
