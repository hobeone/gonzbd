package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestStoreMuLockNesting_MatchesEnumeration enforces Part E Item 1:
// storeMu is acquired only with d.mu released, and d.mu is nested inside
// storeMu at exactly one site (persistIfChanged in tick.go).
// Any d.mu -> storeMu acquisition or a second storeMu -> d.mu site would
// restore the AB-BA deadlock cycle.
func TestStoreMuLockNesting_MatchesEnumeration(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	var muToStoreMu []string
	var storeMuToMu []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse file %s: %v", entry.Name(), err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			var inMu, inStoreMu bool
			fnName := fn.Name.Name

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}

				lockName := ""
				method := sel.Sel.Name
				if method != "Lock" && method != "Unlock" {
					return true
				}

				// Check if selector is d.mu or d.storeMu
				switch inner := sel.X.(type) {
				case *ast.SelectorExpr:
					switch inner.Sel.Name {
					case "mu":
						lockName = "mu"
					case "storeMu":
						lockName = "storeMu"
					}
				case *ast.Ident:
					switch inner.Name {
					case "mu":
						lockName = "mu"
					case "storeMu":
						lockName = "storeMu"
					}
				}

				if lockName == "" {
					return true
				}

				switch method {
				case "Lock":
					switch lockName {
					case "storeMu":
						if inMu {
							muToStoreMu = append(muToStoreMu, fnName)
						}
						inStoreMu = true
					case "mu":
						if inStoreMu {
							storeMuToMu = append(storeMuToMu, fnName)
						}
						inMu = true
					}
				case "Unlock":
					switch lockName {
					case "storeMu":
						inStoreMu = false
					case "mu":
						inMu = false
					}
				}
				return true
			})
		}
	}

	if len(muToStoreMu) != 0 {
		t.Errorf("found d.mu -> storeMu lock nesting in %v; d.mu must NEVER be held when acquiring storeMu (deadlock risk)", muToStoreMu)
	}

	wantStoreMuToMu := []string{"persistIfChanged"}
	slices.Sort(storeMuToMu)
	slices.Sort(wantStoreMuToMu)
	if !slices.Equal(storeMuToMu, wantStoreMuToMu) {
		t.Errorf("storeMu -> d.mu lock nesting sites = %v, want exactly %v", storeMuToMu, wantStoreMuToMu)
	}
}
