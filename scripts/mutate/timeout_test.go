package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTestArgs_BoundsARunTheSpecSetsNoTimeoutFor(t *testing.T) {
	t.Parallel()

	args := testArgs(&spec{pkg: "./p/"})
	i := slices.Index(args, "-timeout")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("testArgs = %q, want -timeout %s when the spec sets none", args, defaultTestTimeout)
	}
	if args[i+1] != defaultTestTimeout.String() {
		t.Errorf("testArgs -timeout = %s, want %s", args[i+1], defaultTestTimeout)
	}

	// A spec's own timeout replaces the default rather than joining it.
	own := testArgs(&spec{pkg: "./p/", timeout: 15 * time.Minute})
	if n := strings.Count(strings.Join(own, " "), "-timeout"); n != 1 {
		t.Errorf("testArgs = %q, want exactly one -timeout", own)
	}
	if !slices.Contains(own, "15m0s") {
		t.Errorf("testArgs = %q, want the spec's 15m0s", own)
	}
}

// hangModule writes a module whose one test waits for Ready, which returns
// ready. With ready false the test never finishes on its own.
func hangModule(t *testing.T, ready bool) string {
	t.Helper()
	root := t.TempDir()
	val := "false"
	if ready {
		val = "true"
	}
	mustWrite(t, filepath.Join(root, "go.mod"), "module hang\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(root, "target.go"), "package hang\n\nfunc Ready() bool {\n\treturn "+val+"\n}\n")
	mustWrite(t, filepath.Join(root, "target_test.go"), "package hang\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\n"+
		"func TestReady(t *testing.T) {\n\tfor !Ready() {\n\t\ttime.Sleep(time.Millisecond)\n\t}\n}\n")
	return root
}

// TestRunSpec_AMutationThatHangsIsKilled pins how a timeout reads during a
// mutation: the test did not pass, so the mutation is caught, and the
// evidence says it was by the deadline rather than an assertion.
func TestRunSpec_AMutationThatHangsIsKilled(t *testing.T) {
	root := hangModule(t, true)
	specPath := filepath.Join(root, "hang.spec")
	mustWrite(t, specPath, "pkg .\nrun TestReady\ntimeout 2s\n\n[never ready]\nfile target.go\n--- anchor\n\treturn true\n--- replace\n\treturn false\n--- end\n")

	var code int
	out, errOut := captureOutput(t, func() {
		code = runSpec(root, specPath, runOpts{skipRunfilter: true})
	})
	if code != 0 {
		t.Fatalf("runSpec = %d, want 0 (the hang is a kill); stdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "KILLED") || !strings.Contains(out, "the mutated test never finished: go test timed out after 2s") {
		t.Errorf("runSpec output lacks a KILLED row naming the timeout:\n%s", out)
	}
}

// TestRunSpec_ABaselineThatTimesOutIsABaselineFailure pins the other side: an
// unmutated test that runs out of time is a broken baseline, reported as one,
// and no mutation is applied — a timeout there must never read as a kill.
func TestRunSpec_ABaselineThatTimesOutIsABaselineFailure(t *testing.T) {
	root := hangModule(t, false)
	specPath := filepath.Join(root, "hang.spec")
	mustWrite(t, specPath, "pkg .\nrun TestReady\ntimeout 2s\n\n[ready]\nfile target.go\n--- anchor\n\treturn false\n--- replace\n\treturn true\n--- end\n")

	var code int
	out, errOut := captureOutput(t, func() {
		code = runSpec(root, specPath, runOpts{skipRunfilter: true})
	})
	if code != 1 {
		t.Errorf("runSpec = %d, want 1 (BASELINE FAILED)", code)
	}
	if !strings.Contains(errOut, "BASELINE FAILED") {
		t.Errorf("stderr lacks BASELINE FAILED:\n%s", errOut)
	}
	if !strings.Contains(errOut, "the unmutated test ran out of time") {
		t.Errorf("stderr does not say the baseline timed out:\n%s", errOut)
	}
	if strings.Contains(out, "KILLED") {
		t.Errorf("a timed-out baseline produced a verdict:\n%s", out)
	}
}

func TestTimedOut(t *testing.T) {
	t.Parallel()

	out := "panic: test timed out after 2m0s\n\trunning tests:\n\t\tTestReady (2m0s)\n\ngoroutine 1 [running]:\nFAIL\thang\t120.004s\n"
	if d, ok := timedOut(out); !ok || d != "2m0s" {
		t.Errorf("timedOut = %q, %v, want 2m0s, true", d, ok)
	}
	// The phrase inside a test's own message is not go test's deadline.
	if _, ok := timedOut("    x_test.go:9: want panic: test timed out after 1s\n--- FAIL: TestX\n"); ok {
		t.Error("timedOut matched a test's own message")
	}
	if _, ok := timedOut(testFailedOutput); ok {
		t.Error("timedOut matched an assertion failure")
	}
}
