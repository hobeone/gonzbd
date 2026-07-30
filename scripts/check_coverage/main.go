// Package main is a dev tool that checks test coverage thresholds for
// functions modified relative to the base branch (origin/main or HEAD~1).
// It exits non-zero if any changed function has 0% coverage or less than 80%.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec" //nolint:gosec // dev tool, fixed command args
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hobeone/gonzbd/scripts/gitscope"
)

type FunctionRange struct {
	Name      string
	StartLine int
	EndLine   int
	NoCover   bool // true if the func line contains //nocover:
}

func main() {
	os.Exit(run())
}

// run executes the coverage check and returns an OS exit code.
// Extracting the logic here ensures deferred cleanup (temp file removal)
// runs before os.Exit is called in main — os.Exit skips defers.
func run() int {
	// 1. Get changed files and changed lines from Git
	changedFiles, err := getChangedLines()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting changed files/lines from git: %v\n", err)
		return 1
	}

	if len(changedFiles) == 0 {
		fmt.Println("No Go files modified relative to base branch.")
		return 0
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
		if err := runPkgCoverage(pkgDir, coverData); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
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
		return 1
	}

	fmt.Println("\nStatus: All modified functions meet the coverage thresholds.")
	return 0
}

// runPkgCoverage runs go test with coverage for pkgDir and populates coverData.
// It returns an error if the tests themselves fail (indicating a build/test problem).
// The temp cover profile is cleaned up before returning.
func runPkgCoverage(pkgDir string, coverData map[string]float64) error {
	hasTests, err := pkgHasTests(pkgDir)
	if err != nil {
		return fmt.Errorf("checking tests in package %s: %w", pkgDir, err)
	}
	if !hasTests {
		return nil
	}

	tmpFile, err := os.CreateTemp("", "gonzbd_cover_*.out")
	if err != nil {
		return fmt.Errorf("creating temp cover profile for package %s: %w", pkgDir, err)
	}
	coverProfile := tmpFile.Name()
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(coverProfile) }() // best-effort cleanup before return

	cmd := exec.Command("go", "test", "-coverprofile="+coverProfile, "./"+pkgDir) //nolint:gosec // dev tool, fixed command args
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tests failed in package %s: %s", pkgDir, stderr.String())
	}

	funcCmd := exec.Command("go", "tool", "cover", "-func="+coverProfile) //nolint:gosec // dev tool, fixed command args
	var out bytes.Buffer
	funcCmd.Stdout = &out
	if err := funcCmd.Run(); err != nil {
		return fmt.Errorf("running go tool cover on %s: %w", pkgDir, err)
	}

	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "total:") || line == "" {
			continue
		}
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
		locParts := strings.Split(location, ":")
		if len(locParts) < 2 {
			continue
		}
		fullPath := locParts[0]
		lineNumStr := locParts[1]
		relPath := fullPath
		const modPrefix = "github.com/hobeone/gonzbd/"
		if after, ok := strings.CutPrefix(relPath, modPrefix); ok {
			relPath = after
		}
		key := fmt.Sprintf("%s:%s:%s", relPath, lineNumStr, funcName)
		coverData[key] = pct
	}
	return nil
}

func getChangedLines() (map[string]map[int]bool, error) {
	// Committed range plus uncommitted work, so the gate gives signal before
	// a commit rather than reporting a vacuous pass. See scripts/gitscope.
	diff, err := gitscope.Diff()
	if err != nil {
		return nil, err
	}

	changedFiles := make(map[string]map[int]bool)
	scanner := bufio.NewScanner(strings.NewReader(diff))
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
			for i := range count {
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
	srcBytes, err := os.ReadFile(path) //nolint:gosec // path comes from git diff output filtered to internal/*.go
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
