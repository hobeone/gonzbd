// Command check_test_doubles prevents test doubles and test-named files
// from leaking into production builds without appropriate build tags.
//
// In Round 3 review (Finding B2 and Part C Class 7), an exported test constructor
// NewTestDurableProof was shipped in internal/durability/testing.go (a non-test
// file without a build tag), which leaked into production builds and completely
// nullified the compile-time DurableProof gate.
//
// This tool performs two AST inspections:
//  1. File name check: Any file under internal/ or cmd/ whose filename matches
//     *test*.go (excluding *_test.go) MUST have a test //go:build tag (test, integration, uitest, crash).
//  2. Identifier check: In any non-test .go file that lacks a test build tag
//     (//go:build test, //go:build integration, //go:build uitest, //go:build crash),
//     inspect AST FuncDecls, TypeDecls, and ValueDecls. Flag any function, method,
//     type, or variable matching ^(New)?(Test|Fake|Mock|Stub|Nop)[A-Z] or
//     .*(ForTesting|ForTests|ForTest)$.
//
// Line-level suppression is supported via comment `//testdouble:allow <reason>`.
// The reason is mandatory; a bare marker is itself reported.
package main

import (
	"cmp"
	"flag"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/hobeone/gonzbd/scripts/gitscope"
)

var (
	testDoubleIdentRe = regexp.MustCompile(`^(New)?(Test|Fake|Mock|Stub|Nop)[A-Z]|.*(ForTesting|ForTests|ForTest)$`)
	testDoubleAllowRe = regexp.MustCompile(`testdouble:allow\s+\S+`)
)

type finding struct {
	file string
	line int
	desc string
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("check_test_doubles", flag.ContinueOnError)
	all := fs.Bool("all", false, "scan all non-test .go files under internal/ and cmd/ instead of the git diff")
	if err := fs.Parse(args); err != nil {
		return 2
	}

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
		fmt.Println("Status: No test double violations found.")
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
	fmt.Printf("\nStatus: Found %d test double violation(s).\n", len(findings))
	return 1
}

// gatherTargetFiles returns the non-test .go files to check: every file under
// internal/ and cmd/ in --all mode, or just the files touched in the current
// change scope otherwise (using gitscope.Files).
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

func checkFile(path string) ([]finding, error) {
	src, err := os.ReadFile(path) //nolint:gosec // dev tool reads repo files
	if err != nil {
		return nil, err
	}
	return checkSource(path, src)
}

func checkSource(path string, src []byte) ([]finding, error) {
	base := filepath.Base(path)
	if strings.HasSuffix(base, "_test.go") {
		return nil, nil
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	commentsByLine := make(map[int]string)
	hasFileLevelSuppression := false
	var findings []finding

	for _, cg := range file.Comments {
		for _, c := range cg.List {
			line := fset.Position(c.Pos()).Line
			commentsByLine[line] += " " + c.Text
			if strings.Contains(c.Text, "testdouble:allow") {
				if !testDoubleAllowRe.MatchString(c.Text) {
					findings = append(findings, finding{
						file: path,
						line: line,
						desc: "//testdouble:allow requires a reason (e.g. //testdouble:allow <reason>)",
					})
				} else if c.Pos() < file.Package {
					hasFileLevelSuppression = true
				}
			}
		}
	}

	hasTestBuildTag := fileHasTestBuildTag(file)

	// 1) File name check: Any file under internal/ or cmd/ whose filename
	// matches *test*.go (excluding *_test.go) MUST have a test //go:build tag
	// (test, integration, uitest, crash).
	matched, _ := filepath.Match("*test*.go", base)
	if matched && !hasTestBuildTag {
		if !hasFileLevelSuppression {
			findings = append(findings, finding{
				file: path,
				line: 1,
				desc: fmt.Sprintf("file %s matches *test*.go but lacks a test //go:build tag (test, integration, uitest, crash)", base),
			})
		}
	}

	// 2) Structural DurableProof encapsulation check:
	// In internal/durability: NO exported function, method, or variable in a non-test file
	// without a test build tag may return, accept, or carry DurableProof.
	// (This structurally closes the class of bug from Round 3 B2 regardless of function name spelling).
	cleanPath := filepath.Clean(filepath.ToSlash(path))
	isDurabilityPkg := strings.HasPrefix(cleanPath, "internal/durability/") || cleanPath == "internal/durability"
	if !hasTestBuildTag && isDurabilityPkg {
		pkgTypeDefs := indexDurabilityTypeDefs(filepath.Dir(cleanPath), file)
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok == token.VAR {
					for _, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range vs.Names {
							if name == nil || !name.IsExported() {
								continue
							}
							carriesProof := false
							if vs.Type != nil {
								carriesProof = returnsDurableProof(vs.Type, pkgTypeDefs, nil)
							} else if i < len(vs.Values) && vs.Values[i] != nil {
								carriesProof = returnsDurableProof(vs.Values[i], pkgTypeDefs, nil)
							}
							if carriesProof {
								findings = append(findings, finding{
									file: path,
									line: fset.Position(name.Pos()).Line,
									desc: fmt.Sprintf("structural encapsulation violation: exported var %s in internal/durability carries DurableProof", name.Name),
								})
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Name == nil || !d.Name.IsExported() || d.Type == nil {
					continue
				}
				kind := "function"
				if d.Recv != nil && len(d.Recv.List) > 0 {
					kind = "method"
				}
				if d.Type.Results != nil {
					for _, result := range d.Type.Results.List {
						if returnsDurableProof(result.Type, pkgTypeDefs, nil) {
							findings = append(findings, finding{
								file: path,
								line: fset.Position(d.Pos()).Line,
								desc: fmt.Sprintf("structural encapsulation violation: exported %s %s in internal/durability returns DurableProof", kind, d.Name.Name),
							})
							break
						}
					}
				}
				if d.Type.Params != nil {
					for _, param := range d.Type.Params.List {
						if returnsDurableProof(param.Type, pkgTypeDefs, nil) {
							findings = append(findings, finding{
								file: path,
								line: fset.Position(d.Pos()).Line,
								desc: fmt.Sprintf("structural encapsulation violation: exported %s %s in internal/durability accepts DurableProof", kind, d.Name.Name),
							})
							break
						}
					}
				}
			}
		}
	}

	// 3) Identifier check: In any non-test .go file that lacks a test build tag
	// (//go:build test, //go:build integration, //go:build uitest, //go:build crash),
	// inspect AST FuncDecls, TypeDecls, and ValueDecls.
	// Flag any symbol matching testDoubleIdentRe unless suppressed.
	if !hasTestBuildTag {
		isSuppressed := func(startLine, declLine int) bool {
			if startLine > 1 {
				startLine--
			}
			for l := startLine; l <= declLine; l++ {
				if testDoubleAllowRe.MatchString(commentsByLine[l]) {
					return true
				}
			}
			return false
		}

		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
				if testDoubleIdentRe.MatchString(fn.Name.Name) {
					startLine := fset.Position(fn.Pos()).Line
					declLine := startLine
					if fn.Doc != nil {
						docStart := fset.Position(fn.Doc.Pos()).Line
						if docStart < startLine {
							startLine = docStart
						}
					}
					if !isSuppressed(startLine, declLine) {
						kind := "function"
						if fn.Recv != nil && len(fn.Recv.List) > 0 {
							kind = "method"
						}
						findings = append(findings, finding{
							file: path,
							line: fset.Position(fn.Pos()).Line,
							desc: fmt.Sprintf("test double %s %s in non-test file without test build tag", kind, fn.Name.Name),
						})
					}
				}
			}

			if gen, ok := decl.(*ast.GenDecl); ok {
				switch gen.Tok {
				case token.TYPE:
					for _, spec := range gen.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || ts.Name == nil {
							continue
						}
						if testDoubleIdentRe.MatchString(ts.Name.Name) {
							startLine := fset.Position(ts.Pos()).Line
							declLine := startLine
							if ts.Doc != nil {
								docStart := fset.Position(ts.Doc.Pos()).Line
								if docStart < startLine {
									startLine = docStart
								}
							} else if gen.Doc != nil {
								docStart := fset.Position(gen.Doc.Pos()).Line
								if docStart < startLine {
									startLine = docStart
								}
							}
							if !isSuppressed(startLine, declLine) {
								findings = append(findings, finding{
									file: path,
									line: fset.Position(ts.Pos()).Line,
									desc: fmt.Sprintf("test double type %s in non-test file without test build tag", ts.Name.Name),
								})
							}
						}
					}
				case token.VAR:
					for _, spec := range gen.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for _, name := range vs.Names {
							if name == nil {
								continue
							}
							if testDoubleIdentRe.MatchString(name.Name) {
								startLine := fset.Position(name.Pos()).Line
								declLine := startLine
								if vs.Doc != nil {
									docStart := fset.Position(vs.Doc.Pos()).Line
									if docStart < startLine {
										startLine = docStart
									}
								} else if gen.Doc != nil {
									docStart := fset.Position(gen.Doc.Pos()).Line
									if docStart < startLine {
										startLine = docStart
									}
								}
								if !isSuppressed(startLine, declLine) {
									findings = append(findings, finding{
										file: path,
										line: fset.Position(name.Pos()).Line,
										desc: fmt.Sprintf("test double var %s in non-test file without test build tag", name.Name),
									})
								}
							}
						}
					}
				}
			}
		}
	}

	return findings, nil
}

func returnsDurableProof(expr ast.Expr, pkgTypeDefs map[string]ast.Expr, visited map[string]bool) bool {
	if expr == nil {
		return false
	}
	if visited == nil {
		visited = make(map[string]bool)
	}
	switch t := expr.(type) {
	case *ast.Ident:
		if t.Name == "DurableProof" {
			return true
		}
		if underlying, ok := pkgTypeDefs[t.Name]; ok && !visited[t.Name] {
			visited[t.Name] = true
			res := returnsDurableProof(underlying, pkgTypeDefs, visited)
			delete(visited, t.Name)
			return res
		}
		return false
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "DurableProof"
	case *ast.StarExpr:
		return returnsDurableProof(t.X, pkgTypeDefs, visited)
	case *ast.ArrayType:
		return returnsDurableProof(t.Elt, pkgTypeDefs, visited)
	case *ast.MapType:
		return returnsDurableProof(t.Key, pkgTypeDefs, visited) || returnsDurableProof(t.Value, pkgTypeDefs, visited)
	case *ast.ChanType:
		return returnsDurableProof(t.Value, pkgTypeDefs, visited)
	case *ast.FuncType:
		if t.Results != nil {
			for _, field := range t.Results.List {
				if returnsDurableProof(field.Type, pkgTypeDefs, visited) {
					return true
				}
			}
		}
		return false
	case *ast.StructType:
		if t.Fields != nil {
			for _, field := range t.Fields.List {
				if returnsDurableProof(field.Type, pkgTypeDefs, visited) {
					return true
				}
			}
		}
		return false
	case *ast.InterfaceType:
		if t.Methods != nil {
			for _, field := range t.Methods.List {
				if returnsDurableProof(field.Type, pkgTypeDefs, visited) {
					return true
				}
			}
		}
		return false
	case *ast.IndexExpr:
		return returnsDurableProof(t.X, pkgTypeDefs, visited) || returnsDurableProof(t.Index, pkgTypeDefs, visited)
	case *ast.IndexListExpr:
		if returnsDurableProof(t.X, pkgTypeDefs, visited) {
			return true
		}
		for _, idx := range t.Indices {
			if returnsDurableProof(idx, pkgTypeDefs, visited) {
				return true
			}
		}
		return false
	case *ast.CallExpr:
		if returnsDurableProof(t.Fun, pkgTypeDefs, visited) {
			return true
		}
		if ident, ok := t.Fun.(*ast.Ident); ok {
			if ident.Name == "newProof" || ident.Name == "DurableProof" {
				return true
			}
		}
		for _, arg := range t.Args {
			if returnsDurableProof(arg, pkgTypeDefs, visited) {
				return true
			}
		}
		return false
	case *ast.CompositeLit:
		if returnsDurableProof(t.Type, pkgTypeDefs, visited) {
			return true
		}
		for _, elt := range t.Elts {
			if returnsDurableProof(elt, pkgTypeDefs, visited) {
				return true
			}
		}
		return false
	case *ast.KeyValueExpr:
		return returnsDurableProof(t.Key, pkgTypeDefs, visited) || returnsDurableProof(t.Value, pkgTypeDefs, visited)
	case *ast.UnaryExpr:
		return returnsDurableProof(t.X, pkgTypeDefs, visited)
	case *ast.ParenExpr:
		return returnsDurableProof(t.X, pkgTypeDefs, visited)
	default:
		return false
	}
}

func indexDurabilityTypeDefs(dir string, file *ast.File) map[string]ast.Expr {
	pkgTypeDefs := make(map[string]ast.Expr)

	scanDir := dir
	if info, err := os.Stat(scanDir); err != nil || !info.IsDir() {
		scanDir = "internal/durability"
	}
	if info, err := os.Stat(scanDir); err == nil && info.IsDir() {
		entries, err := os.ReadDir(scanDir)
		if err == nil {
			fset := token.NewFileSet()
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
					continue
				}
				filePath := filepath.Join(scanDir, entry.Name())
				f, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
				if err != nil {
					continue
				}
				if fileHasTestBuildTag(f) {
					continue
				}
				addTypeDefs(pkgTypeDefs, f)
			}
		}
	}

	if file != nil {
		addTypeDefs(pkgTypeDefs, file)
	}

	return pkgTypeDefs
}

func addTypeDefs(defs map[string]ast.Expr, file *ast.File) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name == nil || ts.Type == nil {
				continue
			}
			defs[ts.Name.Name] = ts.Type
		}
	}
}

func fileHasTestBuildTag(file *ast.File) bool {
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if c.Pos() >= file.Package {
				continue
			}
			text := strings.TrimSpace(c.Text)
			if constraint.IsGoBuild(text) {
				if expr, err := constraint.Parse(text); err == nil {
					if containsTestTag(expr) {
						return true
					}
				}
			}
		}
	}
	return false
}

func containsTestTag(expr constraint.Expr) bool {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		return e.Tag == "test" || e.Tag == "integration" || e.Tag == "uitest" || e.Tag == "crash"
	case *constraint.NotExpr:
		return false
	case *constraint.AndExpr:
		return containsTestTag(e.X) || containsTestTag(e.Y)
	case *constraint.OrExpr:
		return containsTestTag(e.X) && containsTestTag(e.Y)
	default:
		return false
	}
}
