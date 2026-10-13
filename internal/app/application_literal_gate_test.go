package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// appImportPath is this package's import path, as other packages name it.
const appImportPath = "github.com/hobeone/gonzbd/internal/app"

// TestApplicationLiterals_OnlyInNew pins New as the one constructor of an
// Application, in production and test code alike. New always wires the
// dispatcher, and the application's code reads app.dispatcher relying on
// that; an Application built any other way would have a nil one.
//
// It walks every .go file under the module root, skipping directories that
// start with "." (.claude holds worktrees of other branches), node_modules
// and testdata, and reports each composite literal of the type,
// each new() of it and each var declared with it and no value. The one site
// allowed is the literal inside New in app.go.
func TestApplicationLiterals_OnlyInNew(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var sites []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sites = append(sites, applicationConstructions(rel, f)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}

	want := []string{filepath.Join("internal", "app", "app.go") + ":New"}
	if !slices.Equal(sites, want) {
		t.Errorf("Application constructions = %v, want only %v. Build an Application "+
			"with New (newTestApplication in this package's tests): New is what "+
			"guarantees app.dispatcher is set",
			sites, want)
	}
}

// applicationConstructions returns "file:func" for each Application literal,
// new(Application) and zero-value var declaration in f. The type is matched
// as the bare identifier in a file of package app, and through f's own import
// name for internal/app anywhere else.
func applicationConstructions(rel string, f *ast.File) []string {
	inPackage := f.Name.Name == "app" && filepath.Dir(rel) == filepath.Join("internal", "app")
	qualifier := ""
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == appImportPath {
			qualifier = "app"
			if imp.Name != nil {
				qualifier = imp.Name.Name
			}
		}
	}
	isApplication := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return inPackage && x.Name == "Application"
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			return ok && qualifier != "" && id.Name == qualifier && x.Sel.Name == "Application"
		}
		return false
	}

	var out []string
	for _, decl := range f.Decls {
		where := rel + ":" + declName(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if x.Type != nil && isApplication(x.Type) {
					out = append(out, where)
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isApplication(x.Args[0]) {
					out = append(out, where)
				}
			case *ast.ValueSpec:
				if x.Type != nil && len(x.Values) == 0 && isApplication(x.Type) {
					out = append(out, where)
				}
			}
			return true
		})
	}
	return out
}

// declName names a top-level declaration for a report: the function's name,
// or "var"/"const"/"type"/"import" for a GenDecl.
func declName(decl ast.Decl) string {
	if fn, ok := decl.(*ast.FuncDecl); ok {
		return fn.Name.Name
	}
	return decl.(*ast.GenDecl).Tok.String()
}
