package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func chdirRepoRoot(t *testing.T) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get wd: %v", err)
	}
	root := filepath.Join(dir, "..", "..")
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to chdir to repo root: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(dir)
	})
}

func TestRunPkgCoverage_ConcurrentSafety(t *testing.T) {
	chdirRepoRoot(t)

	const numGoroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, numGoroutines)
	results := make([]map[string]float64, numGoroutines)

	for i := range numGoroutines {
		wg.Add(1)
		results[i] = make(map[string]float64)
		go func(idx int) {
			defer wg.Done()
			if err := runPkgCoverage("scripts/gitscope", results[idx]); err != nil {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	var errList []error
	for err := range errs {
		errList = append(errList, err)
	}

	if len(errList) > 0 {
		t.Fatalf("runPkgCoverage failed %d times during concurrent execution: %v", len(errList), errList[0])
	}

	for i, data := range results {
		if len(data) == 0 {
			t.Errorf("goroutine %d returned empty coverData", i)
		}
	}
}

// TestRunPkgCoverage_FailureNamesTheFailingTest pins issue #690: a failing
// `go test -coverprofile` run must surface which test failed, not an empty
// stderr. The throwaway module below is a real `go test` invocation that
// fails for real, rather than a captured fixture of one — what this test
// checks is runPkgCoverage's handling of an actual failing *exec.Cmd, which a
// canned string cannot exercise.
func TestRunPkgCoverage_FailureNamesTheFailingTest(t *testing.T) {
	dir := t.TempDir()
	writeCovTestFile(t, filepath.Join(dir, "go.mod"), "module covtest\n\ngo 1.24\n")
	writeCovTestFile(t, filepath.Join(dir, "pkg", "x_test.go"), `package pkg

import "testing"

func TestWillFail(t *testing.T) {
	t.Fatal("boom")
}
`)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	err = runPkgCoverage("pkg", make(map[string]float64))
	if err == nil {
		t.Fatal("runPkgCoverage returned nil error for a failing test run")
	}
	if !strings.Contains(err.Error(), "TestWillFail") {
		t.Errorf("error = %q, want it to name the failing test TestWillFail (issue #690: an empty stderr named no test)", err.Error())
	}
}

func writeCovTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
