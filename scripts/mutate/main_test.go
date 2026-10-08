package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

	// The root and the escape target are siblings, so "../outside.go" from
	// inside the root names a real file — the containment check has to be
	// what rejects it, not the file's absence.
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(base, "outside.go"), "package outside\n")
	mustWrite(t, filepath.Join(root, "internal", "in.go"), "package in\n")
	mustWrite(t, filepath.Join(root, "..dotfile.go"), "package dot\n")

	// The cost of a typo'd `file ../../etc/thing` is a clobbered file outside
	// the tree, with a backup the author never thinks to look for.
	for _, bad := range []string{"../outside.go", "internal/../../outside.go"} {
		if _, err := resolve(root, bad); err == nil {
			t.Errorf("resolve accepted %q, which escapes the repository", bad)
		}
	}

	got, err := resolve(root, "internal/in.go")
	if err != nil {
		t.Fatalf("resolve rejected a path inside the repo: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "internal", "in.go"))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}

	// A name that merely starts with the same characters as ".." is fine.
	if _, err := resolve(root, "..dotfile.go"); err != nil {
		t.Errorf("resolve rejected %q: %v", "..dotfile.go", err)
	}
}

func TestResolve_RefusesASymlinkOutOfTheRepository(t *testing.T) {
	t.Parallel()

	// Lexical containment passes here: "link.go" contains no "..". Only
	// following the link shows that a write would land outside the tree.
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(base, "outside.go")
	mustWrite(t, outside, "package outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := resolve(root, "link.go"); err == nil {
		t.Error("resolve accepted an in-repo symlink whose target is outside the repository")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
	if !strings.Contains(joined, "-vet=off") {
		t.Errorf("testArgs = %q, missing -vet=off", joined)
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
	if !strings.Contains(bare, "-vet=off") {
		t.Errorf("testArgs = %q, missing -vet=off", bare)
	}
}

func TestTestArgs_PassesParallelOnlyWhenTheSpecSetsIt(t *testing.T) {
	t.Parallel()

	got := strings.Join(testArgs(&spec{pkg: "./p/", parallel: 24}), " ")
	if !strings.Contains(got, "-parallel 24") {
		t.Errorf("testArgs = %q, want -parallel 24", got)
	}

	// Zero must leave go test's default alone rather than pass -parallel 0,
	// which go test rejects.
	if bare := strings.Join(testArgs(&spec{pkg: "./p/"}), " "); strings.Contains(bare, "-parallel") {
		t.Errorf("testArgs = %q, want no -parallel when none was asked for", bare)
	}
}

func TestTestArgs_PassesGcflagsOnlyWhenTheSpecSetsThem(t *testing.T) {
	t.Parallel()

	// One argument, not two: the value holds a space, and -gcflags=-N -l as a
	// single token is what go test reads as the flag with its whole value.
	args := testArgs(&spec{pkg: "./p/", gcflags: "-N -l"})
	if !slices.Contains(args, "-gcflags=-N -l") {
		t.Errorf("testArgs = %q, want the single argument -gcflags=-N -l", args)
	}

	if bare := strings.Join(testArgs(&spec{pkg: "./p/"}), " "); strings.Contains(bare, "-gcflags") {
		t.Errorf("testArgs = %q, want no -gcflags when none was asked for", bare)
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

func TestRanNothing_CatchesARunFilterThatMatchesNoTest(t *testing.T) {
	t.Parallel()

	// Measured: `go test -count=1 . -run TestTypo` prints this and exits 0.
	// Without the check a misspelled run filter yields a green baseline and
	// then reports every mutation SURVIVED against a test that never ran.
	if !ranNothing("ok  \tnotest\t0.001s [no tests to run]\n") {
		t.Error("the [no tests to run] summary was read as a genuine pass")
	}
	if !ranNothing("testing: warning: no tests to run\nPASS\nok \tpkg\t0.1s\n") {
		t.Error("the warning form was read as a genuine pass")
	}
	if ranNothing("ok  \tgithub.com/hobeone/gonzbd/internal/queue\t1.105s\n") {
		t.Error("a real pass was reported as having run nothing")
	}
	// A test whose own message quotes the phrase must not trip it, the same
	// way a quoted [setup failed] must not read as a compile error.
	if ranNothing("--- FAIL: TestX\n    x_test.go:9: wanted [no tests to run] here\nFAIL\n") {
		t.Error("a test message quoting the phrase was read as having run nothing")
	}
}

func TestFirstAssertion_IgnoresSetupLogsBeforeTheFailure(t *testing.T) {
	t.Parallel()

	// t.Log and t.Error format identically, and go test flushes a test's
	// logs with its failure. Scanning from the top hands the evidence column
	// a benign setup line as the reason the mutation was caught.
	out := "=== RUN   TestX\n" +
		"    x_test.go:10: seeding fixture with 20 articles\n" +
		"--- FAIL: TestX (0.09s)\n" +
		"    x_test.go:42: CheckEarlyAbort = true for a paused job\n" +
		"FAIL\n"
	got := firstAssertion(out)
	if want := "x_test.go:42: CheckEarlyAbort = true for a paused job"; got != want {
		t.Errorf("firstAssertion = %q, want %q", got, want)
	}
}

func TestFirstAssertion_MatchesALocationOnItsOwnLine(t *testing.T) {
	t.Parallel()

	// t.Errorf("\ngot %v, want %v", ...) puts the location alone on its line
	// with the message indented beneath. Requiring a trailing space here fell
	// through to the "--- FAIL" banner, which names the test but says nothing
	// about behaviour.
	out := "--- FAIL: TestX (0.00s)\n    x_test.go:42:\n        got 1, want 2\nFAIL\n"
	if got := firstAssertion(out); got != "x_test.go:42:" {
		t.Errorf("firstAssertion = %q, want the bare location line", got)
	}
}

func TestWriteBackup_RemovesItsTempDirWhenTheWriteFails(t *testing.T) {
	// Not parallel: it swaps the package-level writeFile seam.
	var attempted string
	saved := writeFile
	t.Cleanup(func() { writeFile = saved })
	writeFile = func(name string, _ []byte, _ os.FileMode) error {
		attempted = name
		return errors.New("disk full")
	}

	if _, err := writeBackup(filepath.Join(t.TempDir(), "target.go"), []byte("x")); err == nil {
		t.Fatal("writeBackup reported success when the backup write failed")
	}

	// Nothing has registered the directory at this point, so no defer and no
	// signal handler would ever come back for it.
	dir := filepath.Dir(attempted)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("writeBackup left %s behind after a failed write (stat err = %v)", dir, err)
	}
}

func TestGoTest_ReportsALaunchFailureRatherThanAFailedTest(t *testing.T) {
	t.Parallel()

	// A working directory that does not exist makes the child fail to start.
	// The error is not an *exec.ExitError, and reporting it as a non-zero
	// exit would classify the mutation KILLED — counting an unexecuted test
	// as a discriminating pin.
	_, code, launchErr := goTest(filepath.Join(t.TempDir(), "absent"), &spec{pkg: "./..."})
	if launchErr == nil {
		t.Fatal("goTest reported no launch error for an unusable working directory")
	}
	if code != 0 {
		t.Errorf("exitCode = %d alongside a launch error; run() would read that as KILLED", code)
	}
}

func TestNote_ExplainsOnlyTheVerdictsThatGetMisread(t *testing.T) {
	t.Parallel()

	// KILLED needs no note: the evidence column already carries the message,
	// and restating it prints the same text twice.
	if n := note(result{verdict: killed, evidence: "x"}); n != "" {
		t.Errorf("note(KILLED) = %q, want empty", n)
	}
	for _, v := range []verdict{survived, excluded, flaky, compileError, anchorFail} {
		if note(result{verdict: v}) == "" {
			t.Errorf("note(%s) is empty; this verdict is the one that gets misread", v)
		}
	}
	if !strings.Contains(note(result{verdict: compileError}), "says nothing about the") {
		t.Error("the COMPILE_ERROR note does not say the run is not evidence")
	}
}

func TestReportQuiet_OneLineWhenEveryMutationIsKilled(t *testing.T) {
	results := []result{
		{name: "guard neutered", verdict: killed, evidence: "got 1, want 2"},
		{name: "branch deleted", verdict: killed, evidence: "panic: boom"},
	}

	var code int
	out := captureStdout(t, func() {
		code = reportQuiet("internal/x/testdata/x.spec", "", results, 1500*time.Millisecond)
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if want := "ok  \tinternal/x/testdata/x.spec\t1.500s\t2 mutations killed\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	// With chunk label
	outChunk := captureStdout(t, func() {
		code = reportQuiet("internal/x/testdata/x.spec", "1/3", results, 1500*time.Millisecond)
	})
	if want := "ok  \tinternal/x/testdata/x.spec [1/3]\t1.500s\t2 mutations killed\n"; outChunk != want {
		t.Errorf("chunk output = %q, want %q", outChunk, want)
	}
}

func TestReportQuiet_FailureNamesOnlyTheRowsThatDidNotDie(t *testing.T) {
	results := []result{
		{name: "dead one", verdict: killed, evidence: "killed-evidence-text"},
		{name: "live one", verdict: survived, evidence: survivedEvidence},
		{name: "stale one", verdict: anchorFail, evidence: "anchor matched no site"},
	}

	var code int
	out := captureStdout(t, func() {
		code = reportQuiet("internal/x/testdata/x.spec", "", results, 2*time.Second)
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(out, "FAIL\tinternal/x/testdata/x.spec\t2.000s\t2 of 3 mutations did not produce a red result\n") {
		t.Errorf("missing go-test-shaped FAIL header:\n%s", out)
	}
	for _, want := range []string{"live one", "stale one", "SURVIVED", "ANCHOR",
		note(results[1]), note(results[2]),
		"rerun: go run ./scripts/mutate -v internal/x/testdata/x.spec"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"dead one", "killed-evidence-text"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output lists a killed mutation (%q); quiet mode shows failures only:\n%s", unwanted, out)
		}
	}
}

func TestNotKilled_KeepsEveryVerdictButKilledInOrderAndLeavesTheInputAlone(t *testing.T) {
	t.Parallel()

	in := []result{
		{name: "a", verdict: killed},
		{name: "b", verdict: survived},
		{name: "c", verdict: killed},
		{name: "d", verdict: compileError},
	}

	got := notKilled(in)

	if len(got) != 2 || got[0].name != "b" || got[1].name != "d" {
		t.Errorf("notKilled = %v, want rows b and d in order", got)
	}
	if len(in) != 4 || in[0].name != "a" || in[2].name != "c" {
		t.Errorf("notKilled modified its input: %v", in)
	}
}

func TestReport_StillPrintsTheEvidenceOfKilledRows(t *testing.T) {
	results := []result{{name: "dead one", verdict: killed, evidence: "killed-evidence-text"}}

	var code int
	out := captureStdout(t, func() { code = report(results) })

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "killed-evidence-text") || !strings.Contains(out, "Status: all 1 mutations killed") {
		t.Errorf("the default report must keep the evidence column a commit body records:\n%s", out)
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

func captureOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		_ = rOut.Close()
		_ = wOut.Close()
		t.Fatalf("Pipe stderr: %v", err)
	}
	savedOut := os.Stdout
	savedErr := os.Stderr
	os.Stdout = wOut
	os.Stderr = wErr

	var (
		bOut, bErr     bytes.Buffer
		errOut, errErr error
		wg             sync.WaitGroup
	)
	wg.Go(func() {
		_, errOut = bOut.ReadFrom(rOut)
		_ = rOut.Close()
	})
	wg.Go(func() {
		_, errErr = bErr.ReadFrom(rErr)
		_ = rErr.Close()
	})

	closed := false
	finishPipes := func() {
		os.Stdout = savedOut
		os.Stderr = savedErr
		if !closed {
			closed = true
			if err := wOut.Close(); err != nil {
				t.Errorf("close wOut: %v", err)
			}
			if err := wErr.Close(); err != nil {
				t.Errorf("close wErr: %v", err)
			}
			wg.Wait()
		}
	}
	defer finishPipes()

	fn()
	finishPipes()

	if errOut != nil {
		t.Fatalf("read bOut: %v", errOut)
	}
	if errErr != nil {
		t.Fatalf("read bErr: %v", errErr)
	}
	return bOut.String(), bErr.String()
}

func TestRunSpec_SkipBaseline(t *testing.T) {
	// A temporary module with a passing test and a mutation that kills it.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.mod"), "module mutatetest\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(root, "target.go"), "package mutatetest\n\nfunc Value() int {\n\treturn 1\n}\n")
	mustWrite(t, filepath.Join(root, "target_test.go"), "package mutatetest\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(\"value was not 1\")\n\t}\n}\n")

	specContent := "pkg .\nrun TestValue\n\n[the value changed]\nfile target.go\n--- anchor\n\treturn 1\n--- replace\n\treturn 2\n--- end\n"
	specPath := filepath.Join(root, "test.spec")
	mustWrite(t, specPath, specContent)

	// Case 1: skipBaseline = false runs the unmutated baseline first.
	var codeDefault int
	outDefault, _ := captureOutput(t, func() {
		codeDefault = runSpec(root, specPath, runOpts{
			skipBaseline:  false,
			skipRunfilter: true,
		})
	})
	if codeDefault != 0 {
		t.Fatalf("runSpec with baseline = %d, want 0; out:\n%s", codeDefault, outDefault)
	}
	if !strings.Contains(outDefault, "baseline: PASS") {
		t.Errorf("default runSpec output lacks 'baseline: PASS':\n%s", outDefault)
	}
	if !strings.Contains(outDefault, "the value changed") || !strings.Contains(outDefault, "KILLED") {
		t.Errorf("default runSpec output missing mutation verdict:\n%s", outDefault)
	}

	// Case 2: skipBaseline = true skips baseline and still reports mutation verdicts.
	var codeSkip int
	outSkip, _ := captureOutput(t, func() {
		codeSkip = runSpec(root, specPath, runOpts{
			skipBaseline:  true,
			skipRunfilter: true,
		})
	})
	if codeSkip != 0 {
		t.Fatalf("runSpec with skipBaseline = %d, want 0; out:\n%s", codeSkip, outSkip)
	}
	if strings.Contains(outSkip, "baseline:") {
		t.Errorf("skipBaseline runSpec output unexpectedly contains baseline output:\n%s", outSkip)
	}
	if !strings.Contains(outSkip, "the value changed") || !strings.Contains(outSkip, "KILLED") {
		t.Errorf("skipBaseline runSpec output missing mutation verdict:\n%s", outSkip)
	}

	// Case 3: A broken baseline fails when skipBaseline = false, but is bypassed when skipBaseline = true.
	brokenRoot := t.TempDir()
	mustWrite(t, filepath.Join(brokenRoot, "go.mod"), "module brokenmod\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(brokenRoot, "target.go"), "package brokenmod\n\nfunc Broken() int {\n\treturn 1\n}\n")
	mustWrite(t, filepath.Join(brokenRoot, "target_test.go"), "package brokenmod\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) {\n\tt.Fatal(\"broken unmutated test\")\n}\n")
	brokenSpecPath := filepath.Join(brokenRoot, "broken.spec")
	mustWrite(t, brokenSpecPath, "pkg .\nrun TestBroken\n\n[broken mut]\nfile target.go\n--- anchor\n\treturn 1\n--- replace\n\treturn 2\n--- end\n")

	var codeBrokenDefault int
	_, errBrokenDefault := captureOutput(t, func() {
		codeBrokenDefault = runSpec(brokenRoot, brokenSpecPath, runOpts{
			skipBaseline:  false,
			skipRunfilter: true,
		})
	})
	if codeBrokenDefault != 1 {
		t.Errorf("broken baseline with skipBaseline = false code = %d, want 1 (BASELINE FAILED)", codeBrokenDefault)
	}
	if !strings.Contains(errBrokenDefault, "BASELINE FAILED") {
		t.Errorf("broken baseline with skipBaseline = false missing BASELINE FAILED message:\n%s", errBrokenDefault)
	}

	var codeBrokenSkip int
	outBrokenSkip, _ := captureOutput(t, func() {
		codeBrokenSkip = runSpec(brokenRoot, brokenSpecPath, runOpts{
			skipBaseline:  true,
			skipRunfilter: true,
		})
	})
	// With baseline skipped, the mutation runs. Since the test fails on both unmutated and mutated code,
	// the mutation reports KILLED and runSpec exits 0.
	if codeBrokenSkip != 0 {
		t.Errorf("broken baseline with skipBaseline = true code = %d, want 0; out:\n%s", codeBrokenSkip, outBrokenSkip)
	}
	if !strings.Contains(outBrokenSkip, "KILLED") {
		t.Errorf("broken baseline with skipBaseline = true missing KILLED verdict:\n%s", outBrokenSkip)
	}
}

func TestCaptureOutput_DrainsLargeOutputWithoutDeadlock(t *testing.T) {
	// Write 256 KiB (4x the default 64 KiB Linux pipe buffer) to both stdout
	// and stderr inside captureOutput to verify concurrent draining prevents
	// pipe-buffer deadlocks.
	payloadOut := strings.Repeat("o", 256*1024)
	payloadErr := strings.Repeat("e", 256*1024)

	gotOut, gotErr := captureOutput(t, func() {
		_, _ = os.Stdout.WriteString(payloadOut)
		_, _ = os.Stderr.WriteString(payloadErr)
	})
	if len(gotOut) != len(payloadOut) || gotOut != payloadOut {
		t.Errorf("stdout len = %d, want %d", len(gotOut), len(payloadOut))
	}
	if len(gotErr) != len(payloadErr) || gotErr != payloadErr {
		t.Errorf("stderr len = %d, want %d", len(gotErr), len(payloadErr))
	}

	// Verify os.Stdout and os.Stderr are restored even if fn panics.
	origOut, origErr := os.Stdout, os.Stderr
	func() {
		defer func() { _ = recover() }()
		_, _ = captureOutput(t, func() {
			panic("simulated failure inside captureOutput")
		})
	}()
	if os.Stdout != origOut || os.Stderr != origErr {
		t.Errorf("os.Stdout/os.Stderr were not restored after panic inside captureOutput")
	}
}

func TestRunSpec_ReturnsExitCodeTwoOnErrors(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.mod"), "module mutatetest\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(root, "target.go"), "package mutatetest\n\nfunc Value() int {\n\treturn 1\n}\n")

	// Case 1: missing spec file returns exit code 2 without terminating the process.
	var codeMissing int
	_, errMissing := captureOutput(t, func() {
		codeMissing = runSpec(root, filepath.Join(root, "missing.spec"), runOpts{})
	})
	if codeMissing != 2 {
		t.Errorf("missing spec code = %d, want 2", codeMissing)
	}
	if !strings.Contains(errMissing, "mutate:") {
		t.Errorf("missing spec stderr lacks 'mutate:' prefix:\n%s", errMissing)
	}

	// Case 2: invalid chunk flag returns exit code 2 without terminating the process.
	specPath := filepath.Join(root, "test.spec")
	mustWrite(t, specPath, "pkg .\nrun TestValue\n\n[mut]\nfile target.go\n--- anchor\n\treturn 1\n--- replace\n\treturn 2\n--- end\n")

	var codeChunk int
	_, errChunk := captureOutput(t, func() {
		codeChunk = runSpec(root, specPath, runOpts{chunk: "invalid"})
	})
	if codeChunk != 2 {
		t.Errorf("invalid chunk code = %d, want 2", codeChunk)
	}
	if !strings.Contains(errChunk, "invalid -chunk") {
		t.Errorf("invalid chunk stderr lacks 'invalid -chunk':\n%s", errChunk)
	}

	// Case 3: deadRunFilterNames failure (non-existent package) returns exit code 2.
	badPkgSpec := filepath.Join(root, "badpkg.spec")
	mustWrite(t, badPkgSpec, "pkg ./doesnotexist\nrun TestA|TestB\n\n[mut]\nfile target.go\n--- anchor\n\treturn 1\n--- replace\n\treturn 2\n--- end\n")

	var codeRunfilter int
	_, errRunfilter := captureOutput(t, func() {
		codeRunfilter = runSpec(root, badPkgSpec, runOpts{skipRunfilter: false})
	})
	if codeRunfilter != 2 {
		t.Errorf("deadRunFilterNames error code = %d, want 2", codeRunfilter)
	}
	if !strings.Contains(errRunfilter, "list tests") {
		t.Errorf("deadRunFilterNames error stderr lacks 'list tests':\n%s", errRunfilter)
	}
}
