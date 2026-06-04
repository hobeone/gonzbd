package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

func main() {
	var targetFiles []string

	if len(os.Args) > 1 && os.Args[1] == "--all" {
		err := filepath.Walk("internal", func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				targetFiles = append(targetFiles, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error walking internal/: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Get modified Go files via git
		cmd := exec.Command("git", "diff", "--name-only", "origin/main...HEAD")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			// Fallback to checking uncommitted changes if origin/main check fails
			cmd = exec.Command("git", "diff", "--name-only", "HEAD~1")
			stdout.Reset()
			_ = cmd.Run()
		}

		files := strings.SplitSeq(stdout.String(), "\n")
		for f := range files {
			f = strings.TrimSpace(f)
			if !strings.HasPrefix(f, "internal/") || !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
				continue
			}
			// Make sure file exists
			if _, err := os.Stat(f); err == nil {
				targetFiles = append(targetFiles, f)
			}
		}
	}

	if len(targetFiles) == 0 {
		fmt.Println("No source Go files found to check.")
		os.Exit(0)
	}

	hasGaps := false
	for _, srcPath := range targetFiles {
		if err := checkFile(srcPath, &hasGaps); err != nil {
			fmt.Fprintf(os.Stderr, "Error checking %s: %v\n", srcPath, err)
		}
	}

	if hasGaps {
		fmt.Println("\nStatus: Found unit test gaps for unexported helpers.")
		os.Exit(1)
	}
	fmt.Println("\nStatus: All modified unexported helpers have direct test references.")
}

// gapEntry records an untested unexported function and its estimated complexity.
type gapEntry struct {
	name       string
	complexity int
}

func checkFile(srcPath string, hasGaps *bool) error {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, srcPath, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	// Collect unexported functions with their complexity scores.
	type funcInfo struct {
		name string
		decl *ast.FuncDecl
	}
	var unexported []funcInfo
	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		if name != "" && unicode.IsLower(rune(name[0])) {
			unexported = append(unexported, funcInfo{name, fn})
		}
	}

	if len(unexported) == 0 {
		return nil
	}

	// Locate test files in the same directory.
	dir := filepath.Dir(srcPath)
	testFiles, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return err
	}

	// Collect all identifiers from all test files in the package.
	testIdents := make(map[string]bool)
	for _, tf := range testFiles {
		tset := token.NewFileSet()
		tnode, err := parser.ParseFile(tset, tf, nil, 0)
		if err != nil {
			continue
		}
		ast.Inspect(tnode, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				testIdents[ident.Name] = true
			}
			return true
		})
	}

	// Collect gaps and sort by complexity descending so the highest-priority
	// functions appear first. This guides the agent (or human) toward writing
	// tests for the most complex code before trivial one-liners.
	var gaps []gapEntry
	for _, fi := range unexported {
		if !testIdents[fi.name] {
			gaps = append(gaps, gapEntry{fi.name, funcComplexity(fi.decl)})
		}
	}
	if len(gaps) == 0 {
		return nil
	}

	sort.Slice(gaps, func(i, j int) bool {
		return gaps[i].complexity > gaps[j].complexity
	})

	fmt.Printf("\n--- Gaps identified in file: %s ---\n", srcPath)
	for _, g := range gaps {
		fmt.Printf("  ✗ Helper %q is not directly referenced in test files. (complexity: %d)\n", g.name, g.complexity)
	}
	*hasGaps = true
	return nil
}

// funcComplexity estimates the cyclomatic complexity of a function by counting
// branching AST nodes. This is a heuristic equivalent to gocyclo's approach
// and requires no external tools: each if/for/range/switch/select/case adds 1.
func funcComplexity(fn *ast.FuncDecl) int {
	score := 1
	if fn.Body == nil {
		return score
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
			*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt,
			*ast.CaseClause, *ast.CommClause:
			score++
		}
		return true
	})
	return score
}
