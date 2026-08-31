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
// Four verdicts, and the distinction between two of them is the point:
//
//   - KILLED        the test failed, and the failure is quoted so the commit
//     body can record it as AGENTS.md requires
//   - SURVIVED      the test passed; the assertion does not discriminate
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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	anchorFail   verdict = "ANCHOR"
	compileError verdict = "COMPILE_ERROR"
)

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
	timeout   time.Duration
	mutations []mutation
}

// result pairs a mutation with what running it showed.
type result struct {
	mutation
	verdict  verdict
	evidence string
}

// pending records the file currently mutated, so a signal can put it back.
// Mutations run one at a time, so a single slot suffices.
var pending struct {
	sync.Mutex
	path   string
	backup string
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
	out, code := goTest(root, sp)
	if code != 0 {
		fmt.Fprintf(os.Stderr, "\nBASELINE FAILED — no mutation was applied.\n\n"+
			"Every verdict this command produces is a statement about what the\n"+
			"mutation changed. A test that already fails yields KILLED for any\n"+
			"mutation, and none of them mean anything.\n\n%s\n", indent(out))
		os.Exit(1)
	}
	fmt.Println("baseline: PASS")
	fmt.Println()

	results := make([]result, 0, len(sp.mutations))
	for _, m := range sp.mutations {
		results = append(results, run(root, sp, m, *verbose))
	}

	os.Exit(report(results))
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
	if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil { //nolint:gosec // G703: path is checked by resolve to be inside the repository
		fatal("write %s: %v", m.file, err)
	}

	out, code := goTest(root, sp)
	if verbose {
		fmt.Printf("--- %s ---\n%s\n", m.name, indent(out))
	}

	switch {
	case buildFailed(out):
		return result{mutation: m, verdict: compileError, evidence: firstBuildError(out)}
	case code == 0:
		return result{mutation: m, verdict: survived, evidence: "the assertion does not discriminate"}
	default:
		return result{mutation: m, verdict: killed, evidence: firstAssertion(out)}
	}
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
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", file, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s resolves outside the repository", file)
	}
	return abs, nil
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
	if err := os.WriteFile(backup, content, 0o600); err != nil { //nolint:gosec // G703: backup is a path this function just created under os.MkdirTemp
		return "", err
	}
	pending.Lock()
	pending.path, pending.backup = path, backup
	pending.Unlock()
	return backup, nil
}

// readFile is a seam so the read-back below can be made to disagree with what
// was written, which is the one path a test cannot reach by writing files: a
// successful WriteFile followed by different bytes on disk. Same pattern as
// the `var osOpen = os.Open` seams elsewhere in this repository.
var readFile = os.ReadFile

// restore puts the original bytes back and proves it, rather than trusting the
// write's error return. "Exit codes lie; observed state doesn't."
func restore(path, backup string, original []byte) error {
	if err := os.WriteFile(path, original, 0o600); err != nil { //nolint:gosec // G703: path was checked by resolve before the mutation was written
		return err
	}
	got, err := readFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, original) {
		return fmt.Errorf("file differs from the original after restore")
	}
	pending.Lock()
	pending.path, pending.backup = "", ""
	pending.Unlock()
	_ = os.RemoveAll(filepath.Dir(backup)) //nolint:gosec // G703: removes the os.MkdirTemp directory this run created
	return nil
}

// installSignalRestore puts the source file back if the run is interrupted.
// Without it, a Ctrl-C between the write and the deferred restore leaves
// mutated code in the working tree, where it reads as a real edit.
func installSignalRestore() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-ch
		pending.Lock()
		path, backup := pending.path, pending.backup
		pending.Unlock()
		if path != "" {
			if content, err := os.ReadFile(backup); err == nil { //nolint:gosec // G304: backup is our own os.MkdirTemp path
				_ = os.WriteFile(path, content, 0o600) //nolint:gosec // G703: path was checked by resolve before it was mutated
				fmt.Fprintf(os.Stderr, "\ninterrupted: restored %s\n", path)
			} else {
				fmt.Fprintf(os.Stderr, "\ninterrupted: COULD NOT RESTORE %s — recover from %s\n", path, backup)
			}
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
func goTest(root string, sp *spec) (output string, exitCode int) {
	// G204: the package and -run pattern come from a spec file the developer
	// running this command wrote, the same trust level as the shell they typed
	// it in. The binary is always "go".
	cmd := exec.Command("go", testArgs(sp)...) //nolint:gosec // G204: argv comes from the operator's own spec
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			code = ee.ExitCode()
		}
	}
	return string(out), code
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

// assertionRe matches the location a failing test prints for t.Error/t.Fatal.
var assertionRe = regexp.MustCompile(`^\S*\.go:\d+: `)

// firstAssertion pulls out the message the test printed, which is what
// AGENTS.md asks to be recorded: "A red-green claim without the message it
// produced is an assertion, not evidence."
func firstAssertion(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if s := strings.TrimSpace(line); assertionRe.MatchString(s) {
			return s
		}
	}
	for line := range strings.SplitSeq(out, "\n") {
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
// The two it speaks to are the two that get misread. A SURVIVED result is
// about the test, not the code: the mutated behaviour is real and unpinned. A
// COMPILE_ERROR is a red result that is not evidence, and reading it as a
// dead mutant is how a pin that discriminates nothing gets recorded as proven.
func note(r result) string {
	switch r.verdict {
	case survived:
		return "the test passed with the mutation in place, so it does not pin this\n" +
			"  behaviour. Either the assertion is reached by a different path than\n" +
			"  intended, or the condition it needs is never created by the fixture."
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
