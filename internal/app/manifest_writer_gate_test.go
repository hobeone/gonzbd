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
// production code that builds a queue-manifest path to write, so it is the
// only writer of the file hydration reads back after an eviction.
//
// It names the functions whose bodies reference manifestPath at all — a call,
// or the function taken as a value — across this package's non-test files.
// Reads go through openManifestIn and deletes through removeManifestIn, so a
// reference to manifestPath is a write. A second writer is the shape that
// drifted before: AddJob and RetryHistoryJob each carried one, and a cleanup
// fix applied to the first was missed in the second (992d745f).
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
