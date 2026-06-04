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
			if f == "" || !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
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

func checkFile(srcPath string, hasGaps *bool) error {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, srcPath, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	// Find all unexported functions & methods
	var unexported []string
	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		// Unexported function/method starts with a lowercase letter
		if len(name) > 0 && unicode.IsLower(rune(name[0])) {
			unexported = append(unexported, name)
		}
	}

	if len(unexported) == 0 {
		return nil
	}

	// Locate test files in the same directory
	dir := filepath.Dir(srcPath)
	testFiles, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return err
	}

	// Collect all identifiers from all test files in the package
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

	// Check if each unexported function is referenced in tests
	printedHeader := false
	for _, name := range unexported {
		if !testIdents[name] {
			if !printedHeader {
				fmt.Printf("\n--- Gaps identified in file: %s ---\n", srcPath)
				printedHeader = true
			}
			fmt.Printf("  ✗ Helper %q is not directly referenced in test files.\n", name)
			*hasGaps = true
		}
	}

	return nil
}
