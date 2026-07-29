// Command check_test_alignment reports unexported functions and methods under
// internal/ that are never directly referenced from a test file in the same
// package. It is a discovery aid for unit-test gaps, not a coverage oracle: a
// reference satisfies the check regardless of how thoroughly the helper is
// actually exercised. Use --min-complexity to triage the output toward the
// helpers whose logic most warrants a direct test.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/hobeone/gonzbd/scripts/gitscope"
)

func main() {
	all := flag.Bool("all", false, "scan every non-test .go file under internal/ instead of the git diff")
	minComplexity := flag.Int("min-complexity", 0, "only report helpers whose estimated complexity is >= this value")
	flag.Parse()

	var targetFiles []string

	if *all {
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
		// Changed Go files: committed range plus uncommitted work, so the
		// gate gives signal before a commit rather than reporting a vacuous
		// pass. See scripts/gitscope.
		files, err := gitscope.Files()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving changed files: %v\n", err)
			os.Exit(1)
		}
		for _, f := range files {
			if !strings.HasPrefix(f, "internal/") || !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
				continue
			}
			// Deleted files remain in the diff; skip what is no longer on disk.
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
		if err := checkFile(srcPath, *minComplexity, &hasGaps); err != nil {
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

// funcInfo describes one unexported function or method declaration.
type funcInfo struct {
	name     string
	decl     *ast.FuncDecl
	isMethod bool // has a receiver — called as `recv.name(...)`, not `name(...)`
}

// testRefs records how unexported identifiers are referenced across a package's
// test files. The two classes are kept separate so a plain function named
// `load` is not considered "covered" merely because some unrelated test calls a
// *method* `x.load()`, accesses a struct field `.load`, or vice versa. This is
// the bare-name collision that a single flat identifier set silently masks.
type testRefs struct {
	funcRefs   map[string]bool // identifiers used outside selector position: `load`, `load(...)`, `f := load`
	methodRefs map[string]bool // identifiers used as the selector field: `x.load`, `x.load()`
}

func checkFile(srcPath string, minComplexity int, hasGaps *bool) error {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, srcPath, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	srcBytes, err := os.ReadFile(srcPath) //nolint:gosec // CLI script reading repository source files
	if err != nil {
		return err
	}
	srcLines := strings.Split(string(srcBytes), "\n")

	// Collect unexported functions and methods.
	var unexported []funcInfo
	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		if name != "" && unicode.IsLower(rune(name[0])) {
			startPos := fset.Position(fn.Pos())
			if startPos.Line > 0 && startPos.Line <= len(srcLines) {
				line := srcLines[startPos.Line-1]
				if strings.Contains(line, "//nocover:") || strings.Contains(line, "//noalign:") {
					continue
				}
			}
			unexported = append(unexported, funcInfo{
				name:     name,
				decl:     fn,
				isMethod: fn.Recv != nil,
			})
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

	refs := collectTestRefs(testFiles)

	// Collect gaps and sort by complexity descending so the highest-priority
	// functions appear first. This guides the agent (or human) toward writing
	// tests for the most complex code before trivial one-liners.
	var gaps []gapEntry
	for _, fi := range unexported {
		covered := refs.funcRefs[fi.name]
		if fi.isMethod {
			covered = refs.methodRefs[fi.name]
		}
		if covered {
			continue
		}
		complexity := funcComplexity(fi.decl)
		if complexity < minComplexity {
			continue
		}
		gaps = append(gaps, gapEntry{fi.name, complexity})
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

// collectTestRefs parses every test file and records identifier references,
// partitioned into function-position and selector-position uses. An identifier
// that appears as `x.Sel` is recorded only as a method reference; everything
// else is a function reference. This requires two passes per file: the first
// marks the *ast.Ident nodes that sit in selector position (by pointer
// identity), the second records every other identifier as a function reference.
func collectTestRefs(testFiles []string) testRefs {
	refs := testRefs{
		funcRefs:   make(map[string]bool),
		methodRefs: make(map[string]bool),
	}
	for _, tf := range testFiles {
		tset := token.NewFileSet()
		tnode, err := parser.ParseFile(tset, tf, nil, 0)
		if err != nil {
			continue
		}

		selSels := make(map[*ast.Ident]bool)
		ast.Inspect(tnode, func(n ast.Node) bool {
			if se, ok := n.(*ast.SelectorExpr); ok {
				selSels[se.Sel] = true
				refs.methodRefs[se.Sel.Name] = true
			}
			return true
		})
		ast.Inspect(tnode, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && !selSels[id] {
				refs.funcRefs[id.Name] = true
			}
			return true
		})
	}
	return refs
}

// funcComplexity estimates the cyclomatic complexity of a function by counting
// branching AST nodes. This is a heuristic equivalent to gocyclo's approach and
// requires no external tools: the base is 1, each if/for/range/switch/select/
// case adds 1, and each && / || adds 1 (a short-circuit operator is an extra
// decision point, which gocyclo also counts).
func funcComplexity(fn *ast.FuncDecl) int {
	score := 1
	if fn.Body == nil {
		return score
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
			*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt,
			*ast.CaseClause, *ast.CommClause:
			score++
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				score++
			}
		}
		return true
	})
	return score
}
