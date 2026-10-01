package main

import (
	"fmt"
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

// TestRunPkgCoverage_FailureNamesTheFailingTest pins the invariant that a
// failing `go test -coverprofile` run must surface which test failed, not an
// empty stderr. The throwaway module below is a real `go test` invocation
// that fails for real, rather than a captured fixture of one — what this
// test checks is runPkgCoverage's handling of an actual failing *exec.Cmd,
// which a canned string cannot exercise.
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
		t.Errorf("error = %q, want it to name the failing test TestWillFail: go test reports failures on stdout", err.Error())
	}
}

// TestRunPkgCoverage_ForwardsRealStderrOnBuildFailure pins that
// runPkgCoverage forwards the subprocess's real stderr -- not an empty
// string -- into the returned error. A compile error is the cheapest way to
// put real content on `go test`'s stderr: confirmed by hand (`go test` on
// this fixture, stdout and stderr captured separately) that a build failure
// prints only `FAIL covtest/pkg [build failed]` on stdout, naming no symbol,
// while the compiler's diagnostic naming the undefined symbol goes to
// stderr. A test binary's own direct os.Stderr writes do NOT exercise this
// path -- `go test` multiplexes those into its own stdout together with the
// rest of the test binary's output, which is why TestWillFail above is
// provable only via stdout.
func TestRunPkgCoverage_ForwardsRealStderrOnBuildFailure(t *testing.T) {
	dir := t.TempDir()
	writeCovTestFile(t, filepath.Join(dir, "go.mod"), "module covtest\n\ngo 1.24\n")
	writeCovTestFile(t, filepath.Join(dir, "pkg", "x_test.go"), `package pkg

import "testing"

func TestWillFail(t *testing.T) {
	undefinedSymbolXYZ()
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
		t.Fatal("runPkgCoverage returned nil error for a failing build")
	}
	if !strings.Contains(err.Error(), "undefinedSymbolXYZ") {
		t.Errorf("error = %q, want it to forward the compiler's stderr diagnostic naming the broken symbol", err.Error())
	}
}

// TestFormatTestFailure_FAILLineSurvivesTailTruncation pins the `---
// FAIL:` extraction as its own section, independent of the 50-line tail.
// The FAIL line sits 61 lines before the end of stdout -- outside the tail
// window the fixture-correctness assertion below confirms -- so if the
// marker shows up in the result at all, the `--- FAIL lines ---` section put
// it there.
func TestFormatTestFailure_FAILLineSurvivesTailTruncation(t *testing.T) {
	t.Parallel()

	var stdout strings.Builder
	stdout.WriteString("--- FAIL: TestNamedByFailSection (0.00s)\n")
	for i := range 60 {
		fmt.Fprintf(&stdout, "filler line %d\n", i)
	}

	got := formatTestFailure(stdout.String(), "")

	tailStart := strings.Index(got, "--- last 50 lines of stdout ---")
	if tailStart == -1 {
		t.Fatalf("formatTestFailure dropped the tail section entirely: %q", got)
	}
	if strings.Contains(got[tailStart:], "TestNamedByFailSection") {
		t.Fatal("fixture is wrong: the FAIL line fell inside the tail window, so this does not isolate the FAIL section")
	}
	if !strings.Contains(got, "TestNamedByFailSection") {
		t.Errorf("formatTestFailure dropped the --- FAIL line: %q", got)
	}
}

// TestFormatTestFailure_TailTruncatedTo50LinesWhenNoFAILOrPanic pins two
// things at once, both only observable through output with neither a
// `--- FAIL:` line nor a panic: that the tail section exists at all (the
// bottom marker is the only evidence this kind of failure has), and that it
// is actually truncated to the last 50 lines (the top sentinel, 60 lines
// from the end, must not survive).
func TestFormatTestFailure_TailTruncatedTo50LinesWhenNoFAILOrPanic(t *testing.T) {
	t.Parallel()

	var stdout strings.Builder
	stdout.WriteString("TOP_SENTINEL_MUST_BE_TRUNCATED\n")
	for i := range 58 {
		fmt.Fprintf(&stdout, "filler line %d\n", i)
	}
	stdout.WriteString("BOTTOM_MARKER_MUST_SURVIVE")

	got := formatTestFailure(stdout.String(), "")

	if !strings.Contains(got, "BOTTOM_MARKER_MUST_SURVIVE") {
		t.Errorf("formatTestFailure dropped the only evidence a tail-only failure has: %q", got)
	}
	if strings.Contains(got, "TOP_SENTINEL_MUST_BE_TRUNCATED") {
		t.Errorf("formatTestFailure did not truncate the tail to the last 50 lines: %q", got)
	}
}

// TestFormatTestFailure_PanicBlockSurvivesTailTruncation pins the panic
// block as its own section. The panic marker sits 61 lines before the end
// of stdout -- outside the tail window the fixture-correctness assertion
// below confirms -- so if it shows up in the result at all, the `---
// panic ---` section put it there.
func TestFormatTestFailure_PanicBlockSurvivesTailTruncation(t *testing.T) {
	t.Parallel()

	var stdout strings.Builder
	stdout.WriteString("panic: PANIC_MARKER_7f2a\n")
	for i := range 60 {
		fmt.Fprintf(&stdout, "filler line %d\n", i)
	}

	got := formatTestFailure(stdout.String(), "")

	tailStart := strings.Index(got, "--- last 50 lines of stdout ---")
	if tailStart == -1 {
		t.Fatalf("formatTestFailure dropped the tail section entirely: %q", got)
	}
	if strings.Contains(got[tailStart:], "PANIC_MARKER_7f2a") {
		t.Fatal("fixture is wrong: the panic line fell inside the tail window, so this does not isolate the panic section")
	}
	if !strings.Contains(got, "PANIC_MARKER_7f2a") {
		t.Errorf("formatTestFailure dropped the panic block: %q", got)
	}
}

// TestFormatTestFailure_ForwardsNonEmptyStderr pins the stderr section
// itself (the call-site forwarding of a real `go test` child's stderr is
// pinned separately, by TestRunPkgCoverage_ForwardsRealStderrOnBuildFailure,
// which a canned string cannot stand in for).
func TestFormatTestFailure_ForwardsNonEmptyStderr(t *testing.T) {
	t.Parallel()

	got := formatTestFailure("", "STDERR_MARKER_c91d")
	if !strings.Contains(got, "STDERR_MARKER_c91d") {
		t.Errorf("formatTestFailure dropped non-empty stderr: %q", got)
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
