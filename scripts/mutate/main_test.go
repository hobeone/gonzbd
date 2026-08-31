package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The classifier's inputs are real `go test` output. These fixtures were
// captured from actual runs against this repository rather than written from
// memory of what the tool prints — the COMPILE_ERROR/KILLED split is the whole
// point of the verdict set, and it turns on a marker that is not guessable.
const (
	buildFailedOutput = `# github.com/hobeone/gonzbd/scripts/check_review_banner
scripts/check_review_banner/main.go:32:21: undefined: undefinedIdentifierOnPurpose
FAIL	github.com/hobeone/gonzbd/scripts/check_review_banner [build failed]
`

	testFailedOutput = `--- FAIL: TestCheckEarlyAbort_NonResidentDefersRatherThanAborts (0.09s)
    audit_test.go:653: CheckEarlyAbort = true for a paused job
FAIL
FAIL	github.com/hobeone/gonzbd/internal/queue	1.105s
`

	panicOutput = `panic: runtime error: invalid memory address [recovered]
FAIL	github.com/hobeone/gonzbd/internal/queue	0.4s
`
)

func TestBuildFailed_SeparatesACompileErrorFromATestFailure(t *testing.T) {
	t.Parallel()

	// Reporting a build failure as KILLED is a false green for the pin:
	// AGENTS.md says a compile error "does not demonstrate the test would
	// have caught the behaviour".
	if !buildFailed(buildFailedOutput) {
		t.Error("a [build failed] run was not recognised as a compile error")
	}
	if buildFailed(testFailedOutput) {
		t.Error("a genuine test failure was misreported as a compile error")
	}
	if !buildFailed("FAIL\tpkg [setup failed]\n") {
		t.Error("[setup failed] was not recognised; a worktree without ui/dist reports this")
	}
}

func TestBuildFailed_IgnoresTheMarkerInsideATestMessage(t *testing.T) {
	t.Parallel()

	// A failing assertion may quote the phrase. Reading that as a compile
	// error retires a valid red result as "not evidence" — which is how this
	// command's own first self-run misclassified a genuine failure.
	out := "--- FAIL: TestX (0.00s)\n" +
		"    main_test.go:46: [setup failed] was not recognised\n" +
		"FAIL\ngithub.com/hobeone/gonzbd/scripts/mutate\t0.002s\n"
	if buildFailed(out) {
		t.Error("a test whose message quotes [setup failed] was read as a compile error")
	}
}

func TestResolve_RefusesPathsOutsideTheRepository(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// The cost of a typo'd `file ../../etc/thing` is a clobbered file outside
	// the tree, with a backup the author never thinks to look for.
	for _, bad := range []string{"../outside.go", "internal/../../outside.go", ".."} {
		if _, err := resolve(root, bad); err == nil {
			t.Errorf("resolve accepted %q, which escapes the repository", bad)
		}
	}

	got, err := resolve(root, "internal/queue/queue.go")
	if err != nil {
		t.Fatalf("resolve rejected a path inside the repo: %v", err)
	}
	if want := filepath.Join(root, "internal/queue/queue.go"); got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}

	// A path that merely starts with the same characters as ".." is fine.
	if _, err := resolve(root, "..dotfile.go"); err != nil {
		t.Errorf("resolve rejected %q: %v", "..dotfile.go", err)
	}
}

func TestCheckAnchor_RequiresExactlyOneSite(t *testing.T) {
	t.Parallel()

	const content = "if a {\n\treturn 1\n}\nif a {\n\treturn 1\n}\n"

	// Two matches: a replace would silently pick the first, and the red
	// result would be about a site nobody chose.
	err := checkAnchor(content, "if a {\n\treturn 1\n}", "x.go")
	if err == nil {
		t.Fatal("checkAnchor accepted an anchor matching 2 sites")
	}
	if !strings.Contains(err.Error(), "2 sites") {
		t.Errorf("error = %q, want it to report the count", err)
	}

	// Zero matches: usually a stale anchor, which mutates nothing and would
	// otherwise report SURVIVED against unmutated code.
	if err := checkAnchor(content, "if b {", "x.go"); err == nil {
		t.Fatal("checkAnchor accepted an anchor matching no site")
	}

	if err := checkAnchor(content, "return 1\n}\nif a {", "x.go"); err != nil {
		t.Errorf("checkAnchor rejected a unique anchor: %v", err)
	}
}

func TestFirstBuildError_QuotesTheCompilerDiagnostic(t *testing.T) {
	t.Parallel()

	got := firstBuildError(buildFailedOutput)
	want := "scripts/check_review_banner/main.go:32:21: undefined: undefinedIdentifierOnPurpose"
	if got != want {
		t.Errorf("firstBuildError = %q, want %q", got, want)
	}
	if got := firstBuildError("FAIL\tpkg [build failed]\n"); got == "" {
		t.Error("firstBuildError returned empty for output with no diagnostic line")
	}
}

func TestFirstAssertion_PrefersTheTestMessageOverTheFailBanner(t *testing.T) {
	t.Parallel()

	// The assertion line is what AGENTS.md asks to be recorded as evidence.
	// "--- FAIL: TestFoo" names the test but says nothing about behaviour.
	got := firstAssertion(testFailedOutput)
	want := "audit_test.go:653: CheckEarlyAbort = true for a paused job"
	if got != want {
		t.Errorf("firstAssertion = %q, want %q", got, want)
	}
}

func TestFirstAssertion_FallsBackWhenThereIsNoAssertion(t *testing.T) {
	t.Parallel()

	// A panic produces no file.go:line: message, and returning "the test
	// failed" there would hide the cause.
	if got := firstAssertion(panicOutput); !strings.HasPrefix(got, "panic:") {
		t.Errorf("firstAssertion = %q, want the panic line", got)
	}
	if got := firstAssertion("FAIL\tpkg\t0.1s\n"); got != "the test failed" {
		t.Errorf("firstAssertion = %q, want the generic fallback", got)
	}
}

// compileErrRe requires a column, which is what distinguishes a compiler
// diagnostic from a t.Errorf location. Without that, a test failure at
// audit_test.go:653: would be read as a build error.
func TestCompileErrRe_RequiresAColumn(t *testing.T) {
	t.Parallel()

	if compileErrRe.MatchString("audit_test.go:653: some message") {
		t.Error("a t.Errorf location matched the compiler-diagnostic pattern")
	}
	if !compileErrRe.MatchString("main.go:32:21: undefined: x") {
		t.Error("a compiler diagnostic did not match")
	}
}

func TestTestArgs_AlwaysPassesCount1(t *testing.T) {
	t.Parallel()

	// A cached ok reads as "the test does not discriminate" and is the exact
	// opposite of the truth, so this flag is not optional.
	args := testArgs(&spec{pkg: "./p/", run: "TestFoo", timeout: time.Minute})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-count=1") {
		t.Errorf("testArgs = %q, missing -count=1", joined)
	}
	if !strings.Contains(joined, "-run TestFoo") || !strings.Contains(joined, "-timeout 1m0s") {
		t.Errorf("testArgs = %q", joined)
	}

	// An absent run filter must not produce a bare -run with no pattern,
	// which go test reads as the next argument.
	bare := strings.Join(testArgs(&spec{pkg: "./p/"}), " ")
	if strings.Contains(bare, "-run") {
		t.Errorf("testArgs = %q, want no -run when the spec sets none", bare)
	}
	if !strings.Contains(bare, "-count=1") {
		t.Errorf("testArgs = %q, missing -count=1", bare)
	}
}

func TestRestore_ProvesTheBytesRatherThanTrustingTheWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "target.go")
	original := []byte("package p\n\nconst x = 1\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backup, err := writeBackup(path, original)
	if err != nil {
		t.Fatalf("writeBackup: %v", err)
	}

	// The backup lives outside the repository, so a git clean cannot take the
	// only copy of a file that is currently mutated.
	if strings.HasPrefix(backup, mustCwd(t)) {
		t.Errorf("backup %q is inside the working directory", backup)
	}

	if err := os.WriteFile(path, []byte("package p // mutated\n"), 0o600); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if err := restore(path, backup, original); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("after restore the file is %q, want %q", got, original)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Error("the backup outlived a successful restore")
	}
}

func TestRestore_ReportsAReadBackThatDisagrees(t *testing.T) {
	// Not parallel: it swaps the package-level readFile seam.
	path := filepath.Join(t.TempDir(), "target.go")
	original := []byte("package p\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backup, err := writeBackup(path, original)
	if err != nil {
		t.Fatalf("writeBackup: %v", err)
	}

	saved := readFile
	t.Cleanup(func() { readFile = saved })
	readFile = func(string) ([]byte, error) { return []byte("something else\n"), nil }

	// A silent failure here would leave mutated source in the working tree
	// reading as a real edit, which is the worst outcome this command has.
	err = restore(path, backup, original)
	if err == nil {
		t.Fatal("restore reported success when the read-back disagreed with the original")
	}
	if !strings.Contains(err.Error(), "differs from the original") {
		t.Errorf("error = %q, want it to name the mismatch", err)
	}
}

func TestNote_ExplainsOnlyTheVerdictsThatGetMisread(t *testing.T) {
	t.Parallel()

	// KILLED needs no note: the evidence column already carries the message,
	// and restating it prints the same text twice.
	if n := note(result{verdict: killed, evidence: "x"}); n != "" {
		t.Errorf("note(KILLED) = %q, want empty", n)
	}
	for _, v := range []verdict{survived, compileError, anchorFail} {
		if note(result{verdict: v}) == "" {
			t.Errorf("note(%s) is empty; this verdict is the one that gets misread", v)
		}
	}
	if !strings.Contains(note(result{verdict: compileError}), "says nothing about the") {
		t.Error("the COMPILE_ERROR note does not say the run is not evidence")
	}
}

func mustCwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return wd
}
