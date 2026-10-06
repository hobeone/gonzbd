// Command mutate runs the observed red check that AGENTS.md's per-change
// commit cycle mandates in step 2: revert the fix, watch the test fail, put
// the fix back.
//
// That gate had no runner, and AGENTS.md supplied a cp/trap/revert sketch to
// be re-derived per use. Re-derivation is the problem this command exists to
// remove: one session produced eight separate hand-rolled harnesses of this
// shape, and the invariant AGENTS.md stated only as prose — that the anchor
// match exactly once — is the one that failed, in 1 of the 8. A rule re-typed
// from memory per use has a per-use failure rate; the same rule in a runner
// has none. docs/commit-cycle.md § "The red check" holds the measurement;
// AGENTS.md now states the rule and defers the argument to both.
//
// Seven verdicts, and the distinctions between them are the point:
//
//   - KILLED        the test failed, and the failure is quoted so the commit
//     body can record it as AGENTS.md requires
//   - SURVIVED      the test passed; the assertion does not discriminate
//   - EXCLUDED      the test passed, but a package-wide run kills the mutation
//     — so the `run` filter leaves out the test that pins it
//   - FLAKY         the test passed, but a package-wide run kills the
//     mutation by failing a test that `run` DOES select — so the
//     inconsistency is that test's own determinism, not a
//     filter leaving anything out
//   - ANCHOR        the anchor matched zero or several sites, so the mutation
//     is refused rather than applied to a place nobody chose
//   - COMPILE_ERROR the mutated tree does not build, which AGENTS.md warns
//     "does not demonstrate the test would have caught the
//     behaviour" — it is a red result that is not evidence
//   - RUNFILTER     one alternative of the spec's plain-alternation `run`
//     line selects no test in the package — refused before
//     the baseline, for the same
//     reason ANCHOR is refused before a mutation is applied
//
// COMPILE_ERROR is the verdict a hand-rolled script does not have. Reported as
// KILLED it is a false green for the pin: a mutation that breaks the build
// tells you the compiler noticed, never that the test would have.
//
// # A run filter that names no test
//
// `go test -run 'A|B|C'` treats each `|`-separated alternative as an
// independent filter, and an alternative that matches nothing is dropped
// silently — the remaining alternatives still select real tests, the
// baseline still passes, and the phantom name reads as though it were part
// of the pin. Before the baseline runs, deadRunFilterNames lists the
// package's declared tests with `go test -list` and checks every alternative
// of a plain `run` line — one that is nothing but test names joined by `|`,
// in one of three shapes: bare (`A|B`), the whole thing wrapped
// (`^(A|B)$`), or a shared prefix sitting outside the group
// (`<prefix>(A|B)$`, optionally with the leading `^`) — against that list.
// plainAlternation expands each alternative to its full name and reports
// which end(s) the `^`/`$` anchor; selects rebuilds the single-alternative
// pattern -run would have evaluated (prefix+alt, with `^`/`$` added back per
// those flags) and asks the regexp package itself, rather than re-deriving
// containment/equality by hand: unanchored is a substring match, `$` alone
// is a suffix match, and `^`…`$` together is exact equality. A `run` line
// that is not a plain alternation (one carrying other regexp syntax, or a
// subtest path) falls back to the baseline's existing ranNothing check,
// which already refuses a filter that matches nothing at all; what
// RUNFILTER adds is catching the *partial* miss that ranNothing cannot see.
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
// FLAKY is the other way a package-wide run can fail without naming a
// missing term: the test that killed the mutation there is one `run` already
// selects. Reporting that as EXCLUDED sends the reader to widen a filter that
// is not the problem; the filtered run and the package-wide run disagreed
// about the same test, which is a question about that test's determinism.
//
// The baseline is checked before any mutation is applied. A test that is
// already failing produces a KILLED for every mutation, and every one of them
// is meaningless — the whole method rests on the test passing on the fixed
// code first. No hand-rolled script in that session of eight checked this.
//
// # Checking anchors without running anything
//
// -check parses one or more specs, resolves every anchor with the same
// strings.Count checkAnchor uses, and reports any that do not match exactly
// one site — without compiling, running a test, or writing to the working
// tree. -check-all does the same over every spec this checkout has, found
// the way scripts/run_tests.sh finds them: `git ls-files --cached --others
// --exclude-standard -- '*testdata/*.spec'`, never `find` or filepath.Walk —
// both descend into the gitignored `.claude/worktrees/` and would check a
// sibling branch's specs against this tree's source (see run_tests.sh's own
// comment on this, which this command's discovery matches on purpose).
//
// # Restoring
//
// The source file is restored from a copy this command wrote, on every exit
// path including SIGINT, and the restore is verified by comparing bytes rather
// than trusting the write.
//
// Three paths reach it, and the count is the claim: run's deferred restore for
// a normal return, installSignalRestore's handler for a signal, and
// fatalRestoring for an exit between the write and the verdict — os.Exit skips
// the defer, so those exits restore for themselves. That third one is why the
// sequence is a function: the sentence above was false for the whole life of
// this command until a review found the `go test` launch failure exiting
// through plain fatal, one line from a write-failure path that had carried the
// restore inline from the start.
//
// `git stash` and `git checkout --` are both
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
	"strconv"
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
	flaky        verdict = "FLAKY"
	anchorFail   verdict = "ANCHOR"
	compileError verdict = "COMPILE_ERROR"
	runFilter    verdict = "RUNFILTER"
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
	pkg     string
	run     string
	tags    string
	timeout time.Duration
	// parallel is go test's -parallel. It is not read from the spec file: it
	// is a property of the machine running the sweep, set from the command
	// line, and zero leaves go test's default (GOMAXPROCS).
	parallel  int
	mutations []mutation
}

// runOpts is how the command line shapes one spec's run.
type runOpts struct {
	verbose, quiet, skipRunfilter bool
	parallel                      int
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
	quiet := flag.Bool("q", false, "print one line for a spec whose mutations were all killed, and the failing "+
		"rows with a rerun hint for one that was not (for sweeping many specs)")
	check := flag.Bool("check", false, "parse the given spec(s), resolve every anchor, and report any that "+
		"match zero or several sites, without running any test")
	checkAll := flag.Bool("check-all", false, "like -check, but discovers every spec belonging to this "+
		"checkout with git ls-files instead of taking spec paths as arguments")
	skipRunfilter := flag.Bool("skip-runfilter", false, "skip pre-flight check for dead test names in run filter (used when pre-checked by -check-all)")
	parallel := flag.Int("parallel", 0, "pass -parallel N to go test (0 keeps go test's default, GOMAXPROCS)")
	flag.Usage = usage
	flag.Parse()

	if *verbose && *quiet {
		fmt.Fprintln(os.Stderr, "mutate: -v and -q contradict each other; pass one")
		os.Exit(2)
	}
	if *parallel < 0 {
		fmt.Fprintln(os.Stderr, "mutate: -parallel must not be negative")
		os.Exit(2)
	}

	root, err := repoRoot()
	if err != nil {
		fatal("%v", err)
	}

	switch {
	case *checkAll:
		if flag.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "mutate: -check-all takes no spec arguments; it discovers them itself")
			os.Exit(2)
		}
		specs, err := discoverSpecs(root)
		if err != nil {
			fatal("%v", err)
		}
		abs := make([]string, len(specs))
		for i, s := range specs {
			abs[i] = filepath.Join(root, s)
		}
		os.Exit(runCheck(root, specs, abs))
	case *check:
		if flag.NArg() == 0 {
			flag.Usage()
			os.Exit(2)
		}
		os.Exit(runCheck(root, flag.Args(), flag.Args()))
	default:
		if flag.NArg() != 1 {
			flag.Usage()
			os.Exit(2)
		}
		runSpec(root, flag.Arg(0), runOpts{verbose: *verbose, quiet: *quiet, skipRunfilter: *skipRunfilter, parallel: *parallel})
	}
}

// runSpec is the command's original behaviour: apply every mutation in one
// spec, in turn, and require each to produce KILLED.
//
// quiet is the sweep's view of it, in the manner of `go test ./...`: a spec
// that passes is one line, and a spec whose mutations fail prints what failed
// and how to look closer. The evidence column a passing spec produces is what a
// commit body records, so quiet is opt-in rather than the default.
func runSpec(root, path string, opts runOpts) {
	start := time.Now()
	sp, err := parseSpec(path)
	if err != nil {
		fatal("%s: %v", path, err)
	}
	sp.parallel = opts.parallel

	if !opts.skipRunfilter {
		if dead, err := deadRunFilterNames(root, sp); err != nil {
			fatal("%v", err)
		} else if len(dead) > 0 {
			os.Exit(reportRunFilter(sp.pkg, dead))
		}
	}

	installSignalRestore()

	// The baseline runs first and unmutated. Every verdict below is a claim
	// about what the mutation changed, and that claim is empty if the test was
	// not passing to begin with.
	baselineCmd := "go " + strings.Join(testArgs(sp), " ")
	if !opts.quiet {
		fmt.Printf("baseline: %s\n", baselineCmd)
	}
	out, code, launchErr := goTest(root, sp)
	if launchErr != nil {
		fatal("could not run go test: %v", launchErr)
	}
	if code != 0 {
		fmt.Fprintf(os.Stderr, "\nBASELINE FAILED — no mutation was applied.\n\n"+
			"Every verdict this command produces is a statement about what the\n"+
			"mutation changed. A test that already fails yields KILLED for any\n"+
			"mutation, and none of them mean anything.\n\n"+
			"command: %s\n\n%s\n", baselineCmd, indent(out))
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
			"mutation as SURVIVED.\n\n"+
			"command: %s\n\n%s\n", sp.pkg, baselineCmd, indent(out))
		os.Exit(1)
	}
	if !opts.quiet {
		fmt.Println("baseline: PASS")
		fmt.Println()
	}

	results := make([]result, 0, len(sp.mutations))
	for _, m := range sp.mutations {
		results = append(results, run(root, sp, m, opts.verbose))
	}

	confirmed, err := confirmExclusions(root, sp, results)
	if err != nil {
		fatal("%v", err)
	}

	if opts.quiet {
		os.Exit(reportQuiet(path, confirmed, time.Since(start)))
	}
	os.Exit(report(confirmed))
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
		// empty or half-written, and the only good copy in a temp dir nobody
		// was told about.
		fatalRestoring(path, backup, original, "write %s: %v", m.file, err)
	}

	out, code, launchErr := goTest(root, sp)
	if launchErr != nil {
		fatalRestoring(path, backup, original, "could not run go test: %v", launchErr)
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
// behaviour, because the spec's `run` filter excludes the test that does, or
// because a test `run` DOES select disagreed with itself between the two
// runs.
//
// Re-running the same mutation with no filter answers it directly. A package
// that goes red without the filter and green with it holds a test that
// discriminates, and classifyWiderFailure sorts which of the two ways: a test
// `run` excludes (a defect in the spec, EXCLUDED) or a test `run` already
// selects disagreeing with itself between the two runs (a defect in that
// test's own determinism, FLAKY) — both indistinguishable from the SURVIVED
// row alone. The extra run costs nothing on the path that matters: every
// mutation reaching here has already failed the command, so this only ever
// lengthens a run that was going to exit non-zero.
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

	v, ev := classifyWiderFailure(sp.run, out)
	return result{mutation: m, verdict: v, evidence: ev}
}

// classifyWiderFailure turns the package-wide run's failing tests into a
// verdict. A name `run` does not select is a spec defect (EXCLUDED); a name
// `run` DOES select disagreeing with the filtered run it just passed is a
// determinism problem in that test, not in the spec (FLAKY).
//
// Failures split between the two kinds still name a real gap in `run` — the
// excluded test would be missed on every future run of this spec, flaky or
// not — so a mix is reported EXCLUDED, naming only the excluded test(s); a
// FLAKY row would send the reader to look at a test that is not the one
// missing from `run`.
func classifyWiderFailure(run, out string) (v verdict, evidence string) {
	names := failingTests(out)
	if len(names) == 0 {
		return excluded, "the package-wide run fails, so `run` excludes a test that kills this"
	}

	// The selection decision is made on the UN-folded path — `run` can
	// restrict to one subtest (`TestX/subA`), and a sibling subtest
	// (`TestX/subB`) that also failed is excluded even though both fold to
	// the same top-level name. Matching the folded name alone (as a prior
	// version of this function did) reads a `run` line's subtest restriction
	// as selecting the whole parent, and misreports the excluded sibling as
	// this test's own flakiness.
	excludedTop := map[string]bool{}
	for _, p := range failingTestPaths(out) {
		if !filterMatchesName(run, p) {
			top, _, _ := strings.Cut(p, "/")
			excludedTop[top] = true
		}
	}

	var left []string
	for _, n := range names {
		if excludedTop[n] {
			left = append(left, n)
		}
	}

	if len(left) > 0 {
		return excluded, fmt.Sprintf("`run` excludes %s, which kills this", strings.Join(left, ", "))
	}
	return flaky, fmt.Sprintf(
		"%s is selected by `run` and killed this mutation in the package-wide run but not in the filtered run — a determinism problem in the test, not the spec",
		strings.Join(names, ", "))
}

// confirmExclusions checks the other half of what an EXCLUDED or FLAKY row
// claims.
//
// widenOnPass observes that the package is red WITH the mutation. That alone
// does not mean the mutation caused it: a package carrying an unrelated
// failure — a flake, a pre-existing break in a file the spec never names — is
// red either way, and the baseline cannot have caught it, because the baseline
// runs only the filter. So the claim is confirmed against an unmutated,
// unfiltered run, and downgraded to SURVIVED when it does not hold — a FLAKY
// row this way as much as an EXCLUDED one, since both are read off the same
// possibly-unrelated red package.
//
// It runs once per invocation rather than once per mutation, and only when
// something claimed an exclusion or a flake, so a clean spec pays nothing for
// it. It runs after the mutation loop, when every restore has already happened
// and the tree is its real self again.
//
// A confirming run that never STARTS is an error rather than a verdict. It is
// not evidence the package is red — nothing was observed at all — and the two
// must not be conflated, for the same reason goTest separates a launch failure
// from a non-zero exit: reporting an unobserved outcome as a verdict is the
// false green this command exists to refuse.
func confirmExclusions(root string, sp *spec, results []result) ([]result, error) {
	if !needsConfirmation(results) {
		return results, nil
	}

	wide := *sp
	wide.run = ""
	out, code, launchErr := goTest(root, &wide)
	if launchErr != nil {
		return nil, fmt.Errorf("could not run the confirming package-wide test: %w", launchErr)
	}
	if code == 0 && !ranNothing(out) {
		return results, nil
	}

	for i := range results {
		if results[i].verdict == excluded || results[i].verdict == flaky {
			results[i].verdict = survived
			results[i].evidence = survivedEvidence + " (the package is red unmutated too)"
		}
	}
	return results, nil
}

// needsConfirmation reports whether any row claims an exclusion or a flake,
// and is what keeps a spec with neither from paying for the confirming run.
//
// It is a named predicate rather than an inline condition because that is the
// only part of the early return a test can observe: confirmExclusions returns
// the rows unchanged whether it skipped the run or made one and found nothing
// to downgrade, so a behavioural test of the skip passes for the wrong reason.
func needsConfirmation(results []result) bool {
	return slices.ContainsFunc(results, func(r result) bool { return r.verdict == excluded || r.verdict == flaky })
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
	for _, p := range failingTestPaths(out) {
		name, _, _ := strings.Cut(p, "/")
		if name == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// failingTestPaths names every test — top-level or subtest — that failed, as
// `go test` printed it (a subtest keeps its full `Parent/child` path),
// deduplicated and in the order reported. classifyWiderFailure needs this
// un-folded form: matching `run` against the folded parent name alone cannot
// tell "run selects this subtest" apart from "run selects a sibling
// subtest", which is exactly the distinction between FLAKY and EXCLUDED.
func failingTestPaths(out string) []string {
	var names []string
	for _, m := range failingTestRe.FindAllStringSubmatch(out, -1) {
		name := m[1]
		if name == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// fatalRestoring puts the file back and then exits, for the exit paths that
// run between writing the mutation and returning a verdict.
//
// It exists because `fatal` calls os.Exit, which skips run's deferred restore,
// so every early exit in that window has to restore for itself — and one of
// them did not. The write-failure path carried the sequence inline from the
// start; the `go test` launch failure three lines below it did not, and left
// the tree mutated with the only good copy in a temp dir nobody was told
// about. Making the sequence an owner rather than a thing to remember is what
// stops the next early exit added there from repeating it.
func fatalRestoring(path, backup string, original []byte, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if rerr := restore(path, backup, original); rerr != nil {
		fatal("%s\nRESTORE ALSO FAILED: %v\nRecover from %s.", msg, rerr, backup)
	}
	fatal("%s (the file was restored)", msg)
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
	args := []string{"test", "-count=1", "-vet=off", sp.pkg}
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
	if sp.parallel > 0 {
		args = append(args, "-parallel", strconv.Itoa(sp.parallel))
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
	printRows(results)

	if bad := len(notKilled(results)); bad > 0 {
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

// reportQuiet is report for a sweep over many specs, and returns the process
// exit code.
//
// A spec whose mutations were all killed is one `go test`-shaped line. One that
// was not prints a FAIL line, a table of only the rows that did not produce a
// red result — the killed rows are what the spec was supposed to do, and
// listing them buries the ones that were not — the note `note` has for each of
// those rows, and the command that reruns the spec with the full table and
// every go test output.
func reportQuiet(specPath string, results []result, elapsed time.Duration) int {
	bad := notKilled(results)
	secs := elapsed.Seconds()
	if len(bad) == 0 {
		fmt.Printf("ok  \t%s\t%.3fs\t%d mutations killed\n", specPath, secs, len(results))
		return 0
	}

	fmt.Printf("FAIL\t%s\t%.3fs\t%d of %d mutations did not produce a red result\n\n",
		specPath, secs, len(bad), len(results))
	printRows(bad)
	fmt.Printf("\nrerun: go run ./scripts/mutate -v %s\n", specPath)
	return 1
}

// notKilled returns the rows that did not produce a red result.
func notKilled(rs []result) []result {
	return slices.DeleteFunc(slices.Clone(rs), func(r result) bool { return r.verdict == killed })
}

// printRows prints the verdict table for rs followed by each row's note.
func printRows(rs []result) {
	width := len("mutation")
	for _, r := range rs {
		width = max(width, len(r.name))
	}

	fmt.Printf("%-*s  %-13s  %s\n", width, "mutation", "verdict", "evidence")
	fmt.Println(strings.Repeat("-", width+17+60))
	for _, r := range rs {
		fmt.Printf("%-*s  %-13s  %s\n", width, r.name, r.verdict, r.evidence)
	}

	// Only the verdicts that are not self-explanatory get a note. Restating a
	// KILLED line here would just print the evidence column twice.
	for _, r := range rs {
		if n := note(r); n != "" {
			fmt.Printf("\n%s:\n  %s\n", r.name, n)
		}
	}
}

// note explains a verdict whose meaning is not carried by the evidence column.
//
// The five it speaks to are the five that get misread. A SURVIVED result is
// about the test, not the code: the mutated behaviour is real and unpinned. An
// EXCLUDED result is about the spec, not the test. A FLAKY result is about
// neither — it is the selected test's own determinism. A COMPILE_ERROR is a
// red result that is not evidence, and reading it as a dead mutant is how a
// pin that discriminates nothing gets recorded as proven. An ANCHOR result
// means nothing was written at all, which a reader skimming for a red result
// could otherwise mistake for one.
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
	case flaky:
		return "the test named in the evidence column is already selected by `run` —\n" +
			"  widening the filter changes nothing. It killed this mutation once and\n" +
			"  passed on it once, so the inconsistency is in the test, not the spec.\n" +
			"  Investigate that test's determinism before trusting either run of it."
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
	fmt.Fprint(os.Stderr, `usage: go run ./scripts/mutate [-v | -q] [-parallel N] <spec-file>
       go run ./scripts/mutate -check <spec-file>...
       go run ./scripts/mutate -check-all

Runs AGENTS.md's observed red check: apply each mutation, require the test to
fail, restore the file. Exits non-zero unless every mutation is KILLED.

-parallel N passes -parallel N to the go test runs that execute tests: the
baseline, each mutation and the package-wide re-runs. Left unset,
go test uses GOMAXPROCS, which a caller that caps GOMAXPROCS to share a machine
between workers (scripts/run_tests.sh) turns into a cap on tests that only wait.

-q is for sweeping every spec: a spec that passes prints one line, and one that
does not prints only its failing rows and the command to rerun it with the full
table. Without it the evidence column is printed for every mutation, which is
what a commit body records.

-check parses the given spec(s) and reports every anchor that resolves to
zero or several sites, without compiling anything, running a test, or writing
to the working tree. -check-all does the same over every spec this checkout
has, discovered with 'git ls-files --cached --others --exclude-standard --
*testdata/*.spec' — the same command scripts/run_tests.sh uses, and for the
same reason: 'find' and filepath.Walk both descend into the gitignored
.claude/worktrees/ and would check a sibling branch's specs against this
tree's source.

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
