package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type FunctionRange struct {
	Name      string
	StartLine int
	EndLine   int
	NoCover   bool // true if the func line contains //nocover:
}

func main() {
	// 1. Get changed files and changed lines from Git
	changedFiles, err := getChangedLines()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting changed files/lines from git: %v\n", err)
		os.Exit(1)
	}

	if len(changedFiles) == 0 {
		fmt.Println("No Go files modified relative to base branch.")
		os.Exit(0)
	}

	// 2. Identify the unique packages containing changed files
	packages := make(map[string]bool)
	for f := range changedFiles {
		dir := filepath.Dir(f)
		packages[dir] = true
	}

	// 3. For each package, run tests with coverprofile
	coverData := make(map[string]float64) // key: "file:line:func" -> coverage%
	for pkgDir := range packages {
		// Run go test
		coverProfile := filepath.Join(os.TempDir(), "gonzbd_cover.out")
		defer os.Remove(coverProfile)

		// Check if package has test files before running
		hasTests, err := pkgHasTests(pkgDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error checking tests in package %s: %v\n", pkgDir, err)
			continue
		}

		if !hasTests {
			// No tests in package, so all functions in this package will have 0% coverage
			continue
		}

		cmd := exec.Command("go", "test", "-coverprofile="+coverProfile, "./"+pkgDir)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			// If test fails, print output and continue or exit?
			// We exit if the tests themselves fail.
			fmt.Fprintf(os.Stderr, "Tests failed in package %s: %s\n", pkgDir, stderr.String())
			os.Exit(1)
		}

		// Run go tool cover -func
		funcCmd := exec.Command("go", "tool", "cover", "-func="+coverProfile)
		var out bytes.Buffer
		funcCmd.Stdout = &out
		if err := funcCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error running go tool cover on %s: %v\n", pkgDir, err)
			continue
		}

		// Parse go tool cover output
		scanner := bufio.NewScanner(&out)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "total:") || line == "" {
				continue
			}
			// Line format: github.com/hobeone/gonzbd/internal/config/expand.go:10: ExpandPaths 100.0%
			parts := strings.Fields(line)
			if len(parts) < 3 {
				continue
			}
			location := parts[0]
			funcName := parts[1]
			pctStr := strings.TrimSuffix(parts[2], "%")
			pct, err := strconv.ParseFloat(pctStr, 64)
			if err != nil {
				continue
			}

			// extract filepath and line number from location
			// location looks like: github.com/hobeone/gonzbd/internal/config/expand.go:10
			locParts := strings.Split(location, ":")
			if len(locParts) < 2 {
				continue
			}
			fullPath := locParts[0]
			lineNumStr := locParts[1]

			// Strip module prefix to get relative path
			relPath := fullPath
			const modPrefix = "github.com/hobeone/gonzbd/"
			if after, ok := strings.CutPrefix(relPath, modPrefix); ok {
				relPath = after
			}

			key := fmt.Sprintf("%s:%s:%s", relPath, lineNumStr, funcName)
			coverData[key] = pct
		}
	}

	// 4. Process each changed file to find changed functions and check coverage
	hasErrors := false
	for file, changedLines := range changedFiles {
		funcs, err := getFileFunctions(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing file %s: %v\n", file, err)
			continue
		}

		printedHeader := false
		for _, fn := range funcs {
			// Check if function overlaps with changed lines
			isChanged := false
			for line := fn.StartLine; line <= fn.EndLine; line++ {
				if changedLines[line] {
					isChanged = true
					break
				}
			}

			if !isChanged {
				continue
			}

			// Skip functions explicitly marked as exempt from coverage.
			// Usage: func (d dummyEmitter) Broadcast(_ Event) {} //nocover: no-op stub
			if fn.NoCover {
				continue
			}

			// Find coverage for this function
			covKey := fmt.Sprintf("%s:%d:%s", file, fn.StartLine, fn.Name)
			pct, exists := coverData[covKey]
			if !exists {
				// If not found in cover output, it might be 0% (or excluded from coverage)
				pct = 0.0
			}

			// Rule 1: Block if newly added/modified function has 0% coverage
			// Rule 2: Warn/fail if coverage is < 80%
			if pct == 0.0 {
				if !printedHeader {
					fmt.Printf("\n--- Coverage failures in file: %s ---\n", file)
					printedHeader = true
				}
				fmt.Printf("  ✗ Function %q has 0%% coverage (BLOCKED)\n", fn.Name)
				hasErrors = true
			} else if pct < 80.0 {
				if !printedHeader {
					fmt.Printf("\n--- Coverage warnings in file: %s ---\n", file)
					printedHeader = true
				}
				fmt.Printf("  ✗ Function %q has %.1f%% coverage (threshold is 80%%)\n", fn.Name, pct)
				hasErrors = true
			}
		}
	}

	if hasErrors {
		fmt.Println("\nStatus: Build check failed due to coverage thresholds.")
		os.Exit(1)
	}

	fmt.Println("\nStatus: All modified functions meet the coverage thresholds.")
}

func getChangedLines() (map[string]map[int]bool, error) {
	// Try diffing against origin/main first, fallback to HEAD~1
	cmd := exec.Command("git", "diff", "-U0", "origin/main...HEAD")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("git", "diff", "-U0", "HEAD~1")
		out.Reset()
		if err := cmd.Run(); err != nil {
			return nil, err
		}
	}

	changedFiles := make(map[string]map[int]bool)
	scanner := bufio.NewScanner(&out)
	var currentFile string

	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "+++ b/"); ok {
			currentFile = after
			if !strings.HasPrefix(currentFile, "internal/") || !strings.HasSuffix(currentFile, ".go") || strings.HasSuffix(currentFile, "_test.go") {
				currentFile = ""
			}
			continue
		}

		if currentFile == "" {
			continue
		}

		if strings.HasPrefix(line, "@@ ") {
			// @@ -oldStart,oldLen +newStart,newLen @@
			// @@ -oldStart +newStart @@
			parts := strings.Split(line, " ")
			if len(parts) < 3 {
				continue
			}
			newPart := parts[2] // e.g. "+108,3" or "+108"
			if !strings.HasPrefix(newPart, "+") {
				continue
			}
			newPart = strings.TrimPrefix(newPart, "+")

			var start, count int
			var err error
			if strings.Contains(newPart, ",") {
				subParts := strings.Split(newPart, ",")
				start, err = strconv.Atoi(subParts[0])
				if err != nil {
					continue
				}
				count, err = strconv.Atoi(subParts[1])
				if err != nil {
					continue
				}
			} else {
				start, err = strconv.Atoi(newPart)
				if err != nil {
					continue
				}
				count = 1
			}

			if count == 0 {
				continue
			}

			if _, exists := changedFiles[currentFile]; !exists {
				changedFiles[currentFile] = make(map[int]bool)
			}
			for i := 0; i < count; i++ {
				changedFiles[currentFile][start+i] = true
			}
		}
	}

	return changedFiles, nil
}

func getFileFunctions(path string) ([]FunctionRange, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	// Read source lines for //nocover: detection.
	srcBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	srcLines := strings.Split(string(srcBytes), "\n")

	var funcs []FunctionRange
	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		startPos := fset.Position(fn.Pos())
		endPos := fset.Position(fn.End())

		// Check if the func declaration line contains //nocover:
		noCover := false
		if startPos.Line > 0 && startPos.Line <= len(srcLines) {
			line := srcLines[startPos.Line-1]
			noCover = strings.Contains(line, "//nocover:")
		}

		funcs = append(funcs, FunctionRange{
			Name:      fn.Name.Name,
			StartLine: startPos.Line,
			EndLine:   endPos.Line,
			NoCover:   noCover,
		})
	}

	return funcs, nil
}

func pkgHasTests(dir string) (bool, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return false, err
	}
	return len(files) > 0, nil
}
