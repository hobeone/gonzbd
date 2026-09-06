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

// analyzeLockNesting inspects fn for mu <-> storeMu lock nesting.
// It returns function names where mu -> storeMu was observed, and where
// storeMu -> mu was observed.
func analyzeLockNesting(fn *ast.FuncDecl) (muToStoreMu []string, storeMuToMu []string) {
	if fn.Body == nil {
		return nil, nil
	}

	deferredCalls := make(map[*ast.CallExpr]bool)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if def, ok := n.(*ast.DeferStmt); ok {
			deferredCalls[def.Call] = true
		}
		return true
	})

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
		switch method {
		case "Lock", "RLock", "Unlock", "RUnlock":
		default:
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
		case "Lock", "RLock":
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
		case "Unlock", "RUnlock":
			// If this unlock was invoked via defer, the lock remains held
			// until the function returns; do not clear inMu / inStoreMu.
			if deferredCalls[call] {
				return true
			}
			switch lockName {
			case "storeMu":
				inStoreMu = false
			case "mu":
				inMu = false
			}
		}
		return true
	})

	return muToStoreMu, storeMuToMu
}

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

			fnMuToStore, fnStoreToMu := analyzeLockNesting(fn)
			muToStoreMu = append(muToStoreMu, fnMuToStore...)
			storeMuToMu = append(storeMuToMu, fnStoreToMu...)
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

func TestStoreMuLockNesting_CatchesDeferredInversion(t *testing.T) {
	fset := token.NewFileSet()
	const probeSrc = `package testpkg

func (d *Dispatcher) probeMuToStoreMuDeferred() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.storeMu.Lock()
	d.storeMu.Unlock()
}

func (d *Dispatcher) probeMuRLockToStoreMuLockDeferred() {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.storeMu.Lock()
	d.storeMu.Unlock()
}

func (d *Dispatcher) probeStoreMuToMuDeferred() {
	d.storeMu.Lock()
	defer d.storeMu.Unlock()
	d.mu.Lock()
	d.mu.Unlock()
}
`
	f, err := parser.ParseFile(fset, "probe.go", probeSrc, 0)
	if err != nil {
		t.Fatalf("parse probeSrc: %v", err)
	}

	var muToStoreMu []string
	var storeMuToMu []string

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		m2s, s2m := analyzeLockNesting(fn)
		muToStoreMu = append(muToStoreMu, m2s...)
		storeMuToMu = append(storeMuToMu, s2m...)
	}

	wantMuToStoreMu := []string{"probeMuRLockToStoreMuLockDeferred", "probeMuToStoreMuDeferred"}
	slices.Sort(muToStoreMu)
	slices.Sort(wantMuToStoreMu)
	if !slices.Equal(muToStoreMu, wantMuToStoreMu) {
		t.Fatalf("deferred mu->storeMu detection failed: got %v, want %v", muToStoreMu, wantMuToStoreMu)
	}

	wantStoreMuToMu := []string{"probeStoreMuToMuDeferred"}
	slices.Sort(storeMuToMu)
	slices.Sort(wantStoreMuToMu)
	if !slices.Equal(storeMuToMu, wantStoreMuToMu) {
		t.Fatalf("deferred storeMu->mu detection failed: got %v, want %v", storeMuToMu, wantStoreMuToMu)
	}
}
