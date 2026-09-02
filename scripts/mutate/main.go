// Command mutate runs the observed red check that AGENTS.md's per-change
// commit cycle mandates in step 2: revert the fix, watch the test fail, put
// the fix back.
//
// That gate is the only one in the cycle with no runner. AGENTS.md says so
// itself — "Neither has a tool that fails the build, which is exactly why both
// have been skipped in practice while every scripted gate stayed green" — and
// supplies a cp/trap/revert sketch to be re-derived per use. Re-derivation is
// the problem this command exists to remove. One session produced eight
// separate hand-rolled harnesses of this shape; all eight passed -count=1 and
// restored from their own copy, both of which AGENTS.md gives as literal
// copy-pasteable text, and seven of eight checked that the anchor matched
// exactly once, which it gives only as prose. A rule re-typed from memory per
// use has a per-use failure rate; the same rule in a runner has none.
//
// Five verdicts, and the distinctions between them are the point:
//
//   - KILLED        the test failed, and the failure is quoted so the commit
//     body can record it as AGENTS.md requires
//   - SURVIVED      the test passed; the assertion does not discriminate
//   - EXCLUDED      the test passed, but a package-wide run kills the mutation
//     — so the `run` filter leaves out the test that pins it
//   - ANCHOR        the anchor matched zero or several sites, so the mutation
//     is refused rather than applied to a place nobody chose
//   - COMPILE_ERROR the mutated tree does not build, which AGENTS.md warns
//     "does not demonstrate the test would have caught the
//     behaviour" — it is a red result that is not evidence
//
// COMPILE_ERROR is the verdict a hand-rolled script does not have. Reported as
// KILLED it is a false green for the pin: a mutation that breaks the build
// tells you the compiler noticed, never that the test would have.
//
// EXCLUDED separates the two reasons a mutation can pass. `run` is a claim
// about which tests bear on the mutations below it, and it is as live a
// citation as an anchor — but a stale one fails silently where a stale anchor
// does not. The baseline catches a filter that matches NOTHING (see
// ranNothing). It cannot catch a filter that matches five tests and misses the
// sixth: the baseline is green, the mutation reports SURVIVED, and that reads
// as "the assertion is inert" when the truth is "the assertion never ran".
// Both times this happened here, the spec was an alternation that had not
// grown a term when a test was added beside it.
//
// The baseline is checked before any mutation is applied. A test that is
// already failing produces a KILLED for every mutation, and every one of them
// is meaningless — the whole method rests on the test passing on the fixed
// code first. No hand-rolled script in that session of eight checked this.
//
// # Restoring
//
// The source file is restored from a copy this command wrote, on every exit
// path including SIGINT, and the restore is verified by comparing bytes rather
// than trusting the write. `git stash` and `git checkout --` are both
// deliberately unused: AGENTS.md forbids the stash because its stack is shared
// with any other session in the repo, and `git checkout --` would discard
// unrelated uncommitted edits in the same file — which, during a review-fix
// loop, is precisely when this command runs.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// verdict is the outcome of one mutation. Only KILLED is a pass.
type verdict string

const (
	killed       verdict = "KILLED"
	survived     verdict = "SURVIVED"
	excluded     verdict = "EXCLUDED"
	anchorFail   verdict = "ANCHOR"
	compileError verdict = "COMPILE_ERROR"
)

// survivedEvidence is the evidence column for a genuine SURVIVED. It is a
// constant because three paths reach that verdict — the package-wide check
// declining to run, declining to conclude, and confirming nothing — and a
// reader comparing two rows should not have to work out whether two different
// sentences mean the same thing.
const survivedEvidence = "the assertion does not discriminate"

// mutation is one entry in the spec: a named, unique anchor in one file and
// the text to put in its place.
type mutation struct {
	name    string
	file    string
	anchor  string
	replace string
}

// spec is a parsed mutation file.
type spec struct {
	pkg       string
	run       string
	tags      string
	timeout   time.Duration
	mutations []mutation
}

// result pairs a mutation with what running it showed.
type result struct {
	mutation
	verdict  verdict
	evidence string
}

// pending records the file currently mutated, so a signal can put it back,
// plus the cancel func for the running `go test` so an interrupt does not
// orphan a compiler in the background. Mutations run one at a time, so a
// single slot suffices.
//
// The mutex covers the restore itself, not just these fields. SIGINT reaches
// the whole process group, so the child dies, cmd.CombinedOutput returns, and
// the deferred restore starts running at the same moment the handler does —
// two writers to one path, and a race on removing the backup directory out
// from under the other.
var pending struct {
	sync.Mutex
	path     string
	backup   string
	original []byte
	cancel   context.CancelFunc
}

func main() {
	verbose := flag.Bool("v", false, "print the full go test output for every mutation")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	root, err := repoRoot()
	if err != nil {
		fatal("%v", err)
	}

	sp, err := parseSpec(flag.Arg(0))
	if err != nil {
		fatal("%s: %v", flag.Arg(0), err)
	}

	installSignalRestore()

	// The baseline runs first and unmutated. Every verdict below is a claim
	// about what the mutation changed, and that claim is empty if the test was
	// not passing to begin with.
	fmt.Printf("baseline: go %s\n", strings.Join(testArgs(sp), " "))
	out, code, launchErr := goTest(root, sp)
	if launchErr != nil {
		fatal("could not run go test: %v", launchErr)
	}
	if code != 0 {
		fmt.Fprintf(os.Stderr, "\nBASELINE FAILED — no mutation was applied.\n\n"+
			"Every verdict this command produces is a statement about what the\n"+
			"mutation changed. A test that already fails yields KILLED for any\n"+
			"mutation, and none of them mean anything.\n\n%s\n", indent(out))
		os.Exit(1)
	}
	if ranNothing(out) {
		// `go test -run TestTypo` exits 0 and prints "[no tests to run]", so
		// a misspelled run filter reads as a green baseline and then reports
		// every mutation SURVIVED — a full sweep of "nothing pins this",
		// against a test that never executed.
		fmt.Fprintf(os.Stderr, "\nBASELINE RAN NO TESTS — no mutation was applied.\n\n"+
			"go test exited 0 without executing anything, which usually means the\n"+
			"`run` pattern matches no test in %s. Left unchecked this reports every\n"+
			"mutation as SURVIVED.\n\n%s\n", sp.pkg, indent(out))
		os.Exit(1)
	}
	fmt.Println("baseline: PASS")
	fmt.Println()

	results := make([]result, 0, len(sp.mutations))
	for _, m := range sp.mutations {
		results = append(results, run(root, sp, m, *verbose))
	}

	os.Exit(report(confirmExclusions(root, sp, results)))
}

// run applies one mutation, runs the test, and restores the file.
func run(root string, sp *spec, m mutation, verbose bool) result {
	path, err := resolve(root, m.file)
	if err != nil {
		return result{mutation: m, verdict: anchorFail, evidence: err.Error()}
	}
	original, err := os.ReadFile(path) //nolint:gosec // G304: path is checked by resolve to be inside the repository
	if err != nil {
		return result{mutation: m, verdict: anchorFail, evidence: err.Error()}
	}

	if err := checkAnchor(string(original), m.anchor, m.file); err != nil {
		return result{mutation: m, verdict: anchorFail, evidence: err.Error()}
	}

	backup, err := writeBackup(path, original)
	if err != nil {
		fatal("back up %s: %v", m.file, err)
	}
	defer func() {
		if err := restore(path, backup, original); err != nil {
			fatal("RESTORE FAILED for %s: %v\n"+
				"The working tree is left mutated. Recover from %s.", m.file, err, backup)
		}
	}()

	mutated := strings.Replace(string(original), m.anchor, m.replace, 1)
	if err := writeFile(path, []byte(mutated), 0o600); err != nil {
		// WriteFile opens with O_TRUNC, so a failure here can leave the file
		// empty or half-written. fatal calls os.Exit, which skips the defer
		// above, so the restore has to happen before the exit rather than
		// through it — otherwise the tree keeps a truncated source file and
		// the only copy sits in a temp dir nobody was told about.
		if rerr := restore(path, backup, original); rerr != nil {
			fatal("write %s: %v\nRESTORE ALSO FAILED: %v\nRecover from %s.", m.file, err, rerr, backup)
		}
		fatal("write %s: %v (the file was restored)", m.file, err)
	}

	out, code, launchErr := goTest(root, sp)
	if launchErr != nil {
		fatal("could not run go test: %v", launchErr)
	}
	if verbose {
		fmt.Printf("--- %s ---\n%s\n", m.name, indent(out))
	}

	switch {
	case buildFailed(out):
		return result{mutation: m, verdict: compileError, evidence: firstBuildError(out)}
	case code == 0:
		// The mutation is still applied here — the restore is deferred above
		// — which is what lets the package-wide run be a statement about this
		// mutation rather than about the tree in general.
		return widenOnPass(root, sp, m, verbose)
	default:
		return result{mutation: m, verdict: killed, evidence: firstAssertion(out)}
	}
}

// widenOnPass asks why the mutation passed: because nothing pins the
// behaviour, or because the spec's `run` filter excludes the test that does.
//
// Re-running the same mutation with no filter answers it directly. A package
// that goes red without the filter and green with it holds a test that
// discriminates and was not selected, which is a defect in the spec rather
// than in the pin — and the two are indistinguishable from the SURVIVED row
// alone. The extra run costs nothing on the path that matters: every mutation
// reaching here has already failed the command, so this only ever lengthens a
// run that was going to exit non-zero.
func widenOnPass(root string, sp *spec, m mutation, verbose bool) result {
	if sp.run == "" {
		// The command already ran the whole package; there is no wider run to
		// compare against and nothing was excluded.
		return result{mutation: m, verdict: survived, evidence: survivedEvidence}
	}

	wide := *sp
	wide.run = ""
	out, code, launchErr := goTest(root, &wide)
	if verbose {
		fmt.Printf("--- %s (package-wide) ---\n%s\n", m.name, indent(out))
	}
	// A launch failure or a build failure says nothing about which tests
	// discriminate, and neither does a green package. Report what was actually
	// observed — SURVIVED — rather than claiming an exclusion nobody saw.
	if launchErr != nil || code == 0 || buildFailed(out) {
		return result{mutation: m, verdict: survived, evidence: survivedEvidence}
	}

	ev := "the package-wide run fails, so `run` excludes a test that kills this"
	if names := failingTests(out); len(names) > 0 {
		ev = fmt.Sprintf("`run` excludes %s, which kills this", strings.Join(names, ", "))
	}
	return result{mutation: m, verdict: excluded, evidence: ev}
}

// confirmExclusions checks the other half of what an EXCLUDED row claims.
//
// widenOnPass observes that the package is red WITH the mutation. That alone
// does not mean the mutation caused it: a package carrying an unrelated
// failure — a flake, a pre-existing break in a file the spec never names — is
// red either way, and the baseline cannot have caught it, because the baseline
// runs only the filter. So the claim is confirmed against an unmutated,
// unfiltered run, and downgraded to SURVIVED when it does not hold.
//
// It runs once per invocation rather than once per mutation, and only when
// something claimed an exclusion, so a clean spec pays nothing for it. It runs
// after the mutation loop, when every restore has already happened and the
// tree is its real self again.
func confirmExclusions(root string, sp *spec, results []result) []result {
	if !needsConfirmation(results) {
		return results
	}

	wide := *sp
	wide.run = ""
	out, code, launchErr := goTest(root, &wide)
	if launchErr == nil && code == 0 && !ranNothing(out) {
		return results
	}

	for i := range results {
		if results[i].verdict == excluded {
			results[i].verdict = survived
			results[i].evidence = survivedEvidence + " (the package is red unmutated too)"
		}
	}
	return results
}

// needsConfirmation reports whether any row claims an exclusion, and is what
// keeps a spec with none from paying for the confirming run.
//
// It is a named predicate rather than an inline condition because that is the
// only part of the early return a test can observe: confirmExclusions returns
// the rows unchanged whether it skipped the run or made one and found nothing
// to downgrade, so a behavioural test of the skip passes for the wrong reason.
func needsConfirmation(results []result) bool {
	return slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded })
}

// failingTestRe matches the banner `go test` prints for a failing test. It
// appears without -v, which is why the package-wide run does not need one.
var failingTestRe = regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`)

// failingTests names the top-level tests that failed, deduplicated and in the
// order go test reported them.
//
// Subtests are folded into their parent: `--- FAIL: TestX/case` is reported as
// TestX, because a `run` line selects by the parent's name and the parent is
// therefore the term the spec is missing.
func failingTests(out string) []string {
	var names []string
	seen := make(map[string]bool)
	for _, m := range failingTestRe.FindAllStringSubmatch(out, -1) {
		name, _, _ := strings.Cut(m[1], "/")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// resolve turns a spec's file path into an absolute one and requires it to
// land inside the repository.
//
// A spec is written by whoever runs the command, so this is not a trust
// boundary in the way check_citations' operand check is — that one executes
// commands found in comments. It is here because this command WRITES, and the
// cost of a typo'd or copy-pasted `file ../../etc/thing` is a clobbered file
// outside the tree with a backup the author never looks for. Containment turns
// that into a refusal before any byte is written.
func resolve(root, file string) (string, error) {
	abs := filepath.Clean(filepath.Join(root, file))

	// Lexical containment alone is not enough: an in-repository symlink whose
	// target is outside the tree passes the ".." test and then gets written
	// through. Resolve both sides — the root too, since a repository under a
	// symlinked path would otherwise fail its own containment check.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// A path that does not exist cannot be mutated; report it as itself
		// rather than as a containment failure.
		return "", fmt.Errorf("resolve %s: %w", file, err)
	}

	rel, err := filepath.Rel(realRoot, realAbs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", file, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s resolves outside the repository", file)
	}
	return realAbs, nil
}

// checkAnchor requires the anchor to identify exactly one site, and runs
// before anything is written.
//
// This is the invariant with the worst failure mode and the one most often
// dropped when the harness is re-typed per use: AGENTS.md warns that "a
// scripted string-replace can match an identical branch elsewhere in the file
// and produce a red result that proves nothing", and a red result that proves
// nothing is indistinguishable from a red result that proves something. Zero
// matches is the same defect wearing the other face — usually a stale anchor
// left behind by the change under test, which mutates nothing and would report
// SURVIVED against unmutated code.
func checkAnchor(content, anchor, file string) error {
	switch n := strings.Count(content, anchor); n {
	case 1:
		return nil
	case 0:
		return fmt.Errorf("anchor matched no site in %s; it may be stale", file)
	default:
		return fmt.Errorf("anchor matched %d sites in %s, want exactly 1", n, file)
	}
}

// writeBackup copies the file's bytes somewhere outside the repository, so a
// `git clean` or a stray `git checkout` cannot take the only copy.
func writeBackup(path string, content []byte) (string, error) {
	dir, err := os.MkdirTemp("", "mutate-")
	if err != nil {
		return "", err
	}
	backup := filepath.Join(dir, filepath.Base(path)+".bak")
	if err := writeFile(backup, content, 0o600); err != nil {
		// Nothing has registered this directory yet, so no defer and no
		// signal handler will ever come back for it.
		_ = os.RemoveAll(dir)
		return "", err
	}
	pending.Lock()
	pending.path, pending.backup, pending.original = path, backup, content
	pending.Unlock()
	return backup, nil
}

// I/O seams, in the pattern of the `var osOpen = os.Open` seams elsewhere in
// this repository.
//
// Both exist for branches a test cannot reach by writing real files: a
// successful write followed by different bytes on disk, and a write that
// fails inside a directory this process just created. Both branches end with
// mutated source left in the working tree, which is this command's worst
// outcome, so neither should go unpinned for want of a seam.
var (
	readFile  = os.ReadFile
	writeFile = os.WriteFile
)

// restore puts the original bytes back and proves it, rather than trusting the
// write's error return. "Exit codes lie; observed state doesn't."
func restore(path, backup string, original []byte) error {
	pending.Lock()
	defer pending.Unlock()
	return restoreLocked(path, backup, original)
}

// restoreLocked is restore's body, for callers that already hold the lock.
// The signal handler is the other one, and it must not race this.
func restoreLocked(path, backup string, original []byte) error {
	if err := writeFile(path, original, 0o600); err != nil {
		return err
	}
	got, err := readFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, original) {
		return fmt.Errorf("file differs from the original after restore")
	}
	pending.path, pending.backup, pending.original = "", "", nil
	_ = os.RemoveAll(filepath.Dir(backup)) //nolint:gosec // G703: removes the os.MkdirTemp directory this run created
	return nil
}

// installSignalRestore puts the source file back if the run is interrupted.
// Without it, a Ctrl-C between the write and the deferred restore leaves
// mutated code in the working tree, where it reads as a real edit.
func installSignalRestore() {
	ch := make(chan os.Signal, 1)
	// SIGHUP as well: closing a terminal or dropping an SSH session while a
	// mutation is applied would otherwise kill the process by default action,
	// running neither the defer nor this handler and stranding mutated source.
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-ch
		pending.Lock()
		// Stop the child compiler before restoring. os.Exit does not wait on
		// or signal children, so without this a `go test` keeps running
		// against the restored tree, competing for the build cache.
		if pending.cancel != nil {
			pending.cancel()
		}
		path, backup, original := pending.path, pending.backup, pending.original
		var err error
		if path != "" {
			err = restoreLocked(path, backup, original)
		}
		pending.Unlock()

		switch {
		case path == "":
			// Nothing was mutated; there is nothing to say.
		case err != nil:
			// Reporting a restore that did not happen is worse than not
			// reporting one: the mutated source stays in the tree reading as
			// a real edit, and the author has been told it is gone.
			fmt.Fprintf(os.Stderr, "\ninterrupted: COULD NOT RESTORE %s: %v\n"+
				"recover from %s\n", path, err, backup)
		default:
			fmt.Fprintf(os.Stderr, "\ninterrupted: restored %s\n", path)
		}
		if s, ok := sig.(syscall.Signal); ok {
			os.Exit(int(s) | 0x80)
		}
		os.Exit(1)
	}()
}

// testArgs builds the go test argv, so the baseline banner prints the command
// that actually ran rather than a hand-written approximation of it.
func testArgs(sp *spec) []string {
	args := []string{"test", "-count=1", sp.pkg}
	if sp.tags != "" {
		// test/integration, test/uitest and test/crash are all behind
		// //go:build tags, so without this no pin in any of them can be
		// red-checked by this command.
		args = append(args, "-tags="+sp.tags)
	}
	if sp.run != "" {
		args = append(args, "-run", sp.run)
	}
	if sp.timeout > 0 {
		args = append(args, "-timeout", sp.timeout.String())
	}
	return args
}

// goTest runs the spec's test. -count=1 is not optional: Go caches a passing
// result keyed on the binary and its inputs, so a mutation run without it can
// replay the pre-mutation pass and report ok — which reads as "the test does
// not discriminate" and is the exact opposite of the truth.
func goTest(root string, sp *spec) (output string, exitCode int, launchErr error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// G204: the package and -run pattern come from a spec file the developer
	// running this command wrote, the same trust level as the shell they typed
	// it in. The binary is always "go".
	cmd := exec.CommandContext(ctx, "go", testArgs(sp)...) //nolint:gosec // G204: argv comes from the operator's own spec
	cmd.Dir = root

	pending.Lock()
	pending.cancel = cancel
	pending.Unlock()
	defer func() {
		pending.Lock()
		pending.cancel = nil
		pending.Unlock()
	}()

	out, err := cmd.CombinedOutput()
	if err != nil {
		// A failure to START the process — go not on PATH, EAGAIN, EACCES —
		// is not an ExitError and says nothing about the test. Returning it
		// as a non-zero exit would classify the mutation KILLED and count an
		// unexecuted test as a discriminating pin, which is the same false
		// green COMPILE_ERROR exists to prevent.
		ee, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			return string(out), 0, err
		}
		return string(out), ee.ExitCode(), nil
	}
	return string(out), 0, nil
}

// buildFailedRe matches the summary line `go test` prints instead of running
// anything when a package does not compile:
//
//	FAIL	github.com/hobeone/gonzbd/internal/queue [build failed]
//
// It is anchored to the start of a line and requires the FAIL summary rather
// than scanning the output for the bracketed phrase, because a failing test's
// own message can contain it. That is not hypothetical: this command's first
// self-run misreported a real test failure as COMPILE_ERROR, because the
// assertion that failed said "[setup failed] was not recognised". Since
// COMPILE_ERROR is the verdict meaning "this run is not evidence", a substring
// scan silently throws away a valid red result — the exact outcome the verdict
// exists to prevent.
var buildFailedRe = regexp.MustCompile(`(?m)^FAIL\s+\S+\s+\[(?:build|setup) failed\]`)

func buildFailed(out string) bool {
	return buildFailedRe.MatchString(out)
}

// compileErrRe matches a Go compiler diagnostic: file.go:line:col: message.
// The column is what separates it from a test's own t.Errorf location, which
// carries only file.go:line:.
var compileErrRe = regexp.MustCompile(`^\S*\.go:\d+:\d+: .+`)

func firstBuildError(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if s := strings.TrimSpace(line); compileErrRe.MatchString(s) {
			return s
		}
	}
	return "the package does not compile"
}

// ranNothingRe matches go test's summary for a package where the -run filter
// selected nothing. The exit status is 0, so only the output distinguishes it
// from a genuine pass.
var ranNothingRe = regexp.MustCompile(`(?m)^(?:ok|\?)\s+\S+.*\[no tests to run\]`)

func ranNothing(out string) bool {
	return ranNothingRe.MatchString(out) || strings.Contains(out, "warning: no tests to run")
}

// assertionRe matches the location a failing test prints for t.Error/t.Fatal.
//
// The trailing separator is optional: `t.Errorf("\ngot %v", got)` makes the
// testing package emit `file.go:12:` alone on its line, with the message
// indented beneath, and requiring a space there dropped to the "--- FAIL"
// banner — which names the test but says nothing about behaviour.
var assertionRe = regexp.MustCompile(`^\S*\.go:\d+:(?:\s|$)`)

// firstAssertion pulls out the message the test printed, which is what
// AGENTS.md asks to be recorded: "A red-green claim without the message it
// produced is an assertion, not evidence."
func firstAssertion(out string) string {
	lines := strings.Split(out, "\n")

	// Scan from the first "--- FAIL" rather than from the top. t.Log and
	// t.Error format identically as file.go:line: message, and go test
	// flushes a test's logs together with its failure, so a fixture that logs
	// its setup would otherwise donate the evidence line — a benign setup
	// message printed as the reason the mutation was caught.
	start := 0
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--- FAIL") {
			start = i + 1
			break
		}
	}
	for _, line := range lines[start:] {
		if s := strings.TrimSpace(line); assertionRe.MatchString(s) {
			return s
		}
	}
	for _, line := range lines {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "--- FAIL") || strings.HasPrefix(s, "panic:") {
			return s
		}
	}
	return "the test failed"
}

// report prints the table and returns the process exit code.
func report(results []result) int {
	width := len("mutation")
	for _, r := range results {
		if len(r.name) > width {
			width = len(r.name)
		}
	}

	fmt.Printf("%-*s  %-13s  %s\n", width, "mutation", "verdict", "evidence")
	fmt.Println(strings.Repeat("-", width+17+60))
	bad := 0
	for _, r := range results {
		if r.verdict != killed {
			bad++
		}
		fmt.Printf("%-*s  %-13s  %s\n", width, r.name, r.verdict, r.evidence)
	}

	// Only the verdicts that are not self-explanatory get a note. Restating a
	// KILLED line here would just print the evidence column twice.
	for _, r := range results {
		if n := note(r); n != "" {
			fmt.Printf("\n%s:\n  %s\n", r.name, n)
		}
	}

	if bad > 0 {
		fmt.Printf("\nStatus: %d of %d mutations did not produce a red result.\n", bad, len(results))
		return 1
	}
	// AGENTS.md: "Record the observed failure message in the commit body or PR.
	// A red-green claim without the message it produced is an assertion, not
	// evidence." The evidence column is that message.
	fmt.Printf("\nStatus: all %d mutations killed; the assertions discriminate.\n"+
		"Record the evidence column in the commit body — a red-green claim without\n"+
		"the message it produced is an assertion, not evidence.\n", len(results))
	return 0
}

// note explains a verdict whose meaning is not carried by the evidence column.
//
// The three it speaks to are the three that get misread. A SURVIVED result is
// about the test, not the code: the mutated behaviour is real and unpinned. An
// EXCLUDED result is about the spec, not the test. A COMPILE_ERROR is a red
// result that is not evidence, and reading it as a dead mutant is how a pin
// that discriminates nothing gets recorded as proven.
func note(r result) string {
	switch r.verdict {
	case survived:
		return "the test passed with the mutation in place, so it does not pin this\n" +
			"  behaviour. Either the assertion is reached by a different path than\n" +
			"  intended, or the condition it needs is never created by the fixture."
	case excluded:
		return "the spec's `run` line, not the test, is what failed here: the package\n" +
			"  kills this mutation and the filter does not select the test that does.\n" +
			"  Add the missing term to `run` and re-run — until then this mutation was\n" +
			"  evaluated against tests that never executed it."
	case compileError:
		return "the mutated tree does not build, so this run says nothing about the\n" +
			"  test. Neuter the condition rather than deleting the block, then re-run."
	case anchorFail:
		return "refused before writing anything. Anchor on text unique to the target."
	default:
		return ""
	}
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mutate: "+format+"\n", args...)
	os.Exit(2)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: go run ./scripts/mutate [-v] <spec-file>

Runs AGENTS.md's observed red check: apply each mutation, require the test to
fail, restore the file. Exits non-zero unless every mutation is KILLED.

Spec format — line-oriented, so multi-line tab-indented Go needs no escaping:

    pkg ./internal/queue/
    run TestCheckEarlyAbort_NonResidentDefersRatherThanAborts
    timeout 15m

    [the guard neutered]
    file internal/example/thing.go
    --- anchor
    	if deadline.IsZero() {
    --- replace
    	if true {
    --- end

The anchor and replacement above are illustrative and deliberately name no real
symbol: this file is Go source, so any repository identifier quoted here is
counted by check_citations against the citation that enumerates it.

Outside a block, blank lines and lines starting with # are ignored. Between
--- anchor and --- replace, and between --- replace and --- end, every line is
taken literally.
`)
}
