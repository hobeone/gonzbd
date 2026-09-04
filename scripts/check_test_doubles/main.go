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
//     *test*.go (excluding *_test.go) MUST have a //go:build tag (e.g. //go:build test).
//  2. Identifier check: In any non-test .go file that lacks a test build tag
//     (//go:build test, //go:build integration, //go:build uitest, //go:build crash),
//     inspect AST FuncDecls. Flag any function or method matching
//     ^(New)?(Test|Fake|Mock|Stub|Nop)[A-Z].
//
// Line-level suppression is supported via comment `//testdouble:allow <reason>`.
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

var testDoubleIdentRe = regexp.MustCompile(`^(New)?(Test|Fake|Mock|Stub|Nop)[A-Z]`)

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
	dir := filepath.Base(filepath.Dir(path))
	if strings.HasSuffix(dir, "test") {
		return nil, nil
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	commentsByLine := make(map[int]string)
	hasFileLevelSuppression := false
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			line := fset.Position(c.Pos()).Line
			commentsByLine[line] += " " + c.Text
			if strings.Contains(c.Text, "testdouble:allow") {
				hasFileLevelSuppression = true
			}
		}
	}

	hasGoBuild := false
	hasTestBuildTag := false

	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if c.Pos() >= file.Package {
				continue
			}
			text := strings.TrimSpace(c.Text)
			if constraint.IsGoBuild(text) {
				hasGoBuild = true
				if expr, err := constraint.Parse(text); err == nil {
					if containsTestTag(expr) {
						hasTestBuildTag = true
					}
				}
			}
		}
	}

	var findings []finding

	// 1) File name check: Any file under internal/ or cmd/ whose filename
	// matches *test*.go (excluding *_test.go) MUST have a //go:build tag (e.g. //go:build test).
	matched, _ := filepath.Match("*test*.go", base)
	if matched && !hasGoBuild {
		if !hasFileLevelSuppression {
			findings = append(findings, finding{
				file: path,
				line: 1,
				desc: fmt.Sprintf("file %s matches *test*.go but lacks a //go:build tag", base),
			})
		}
	}

	// 2) Identifier check: In any non-test .go file that lacks a test build tag
	// (//go:build test, //go:build integration, //go:build uitest, //go:build crash), inspect AST FuncDecls.
	// Flag any function or method matching ^(New)?(Test|Fake|Mock|Stub|Nop)[A-Z].
	if !hasTestBuildTag {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			if testDoubleIdentRe.MatchString(fn.Name.Name) {
				startLine := fset.Position(fn.Pos()).Line
				endLine := fset.Position(fn.End()).Line
				if fn.Doc != nil {
					docStart := fset.Position(fn.Doc.Pos()).Line
					if docStart < startLine {
						startLine = docStart
					}
				}
				if startLine > 1 {
					startLine--
				}

				suppressed := false
				for l := startLine; l <= endLine; l++ {
					if strings.Contains(commentsByLine[l], "testdouble:allow") {
						suppressed = true
						break
					}
				}
				if suppressed {
					continue
				}

				fnPos := fset.Position(fn.Pos())
				kind := "function"
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					kind = "method"
				}
				findings = append(findings, finding{
					file: path,
					line: fnPos.Line,
					desc: fmt.Sprintf("test double %s %s in non-test file without test build tag", kind, fn.Name.Name),
				})
			}
		}
	}

	return findings, nil
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
		return containsTestTag(e.X) || containsTestTag(e.Y)
	default:
		return false
	}
}
