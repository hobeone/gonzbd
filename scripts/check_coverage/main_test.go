package main

import (
	"os"
	"path/filepath"
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
