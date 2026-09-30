package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The package-wide fallback. A `run` filter that selects five tests and misses
// the sixth leaves a green baseline, so the baseline's ranNothing check cannot
// see it; only a wider run tells "nothing pins this" apart from "the test that
// pins this was never selected".

const excludedFailureOutput = `--- FAIL: TestVerifyIdentified (0.00s)
    verify_identified_test.go:44: Unverified = 0, want 2
--- FAIL: TestVerifyIdentified/ambiguous_basename (0.00s)
    verify_identified_test.go:51: Matched = 0, want 2
--- FAIL: TestAssess_Relocated (0.01s)
    assess_relocate_test.go:19: applied 2 renames, want 1
FAIL
FAIL	github.com/hobeone/gonzbd/internal/par2	1.2s
`

func TestFailingTests_NamesEachParentOnce(t *testing.T) {
	t.Parallel()

	got := failingTests(excludedFailureOutput)
	want := []string{"TestVerifyIdentified", "TestAssess_Relocated"}
	if !slices.Equal(got, want) {
		t.Errorf("failingTests = %v, want %v", got, want)
	}
}

func TestFailingTests_FoldsASubtestIntoItsParent(t *testing.T) {
	t.Parallel()

	// A `run` line selects by the parent's name, so the parent is the term the
	// spec is missing. Reporting "TestX/case" names something that cannot be
	// pasted into a run filter as it stands.
	if got := failingTests("--- FAIL: TestX/case (0.00s)\n"); !slices.Equal(got, []string{"TestX"}) {
		t.Errorf("failingTests = %v, want [TestX]", got)
	}
	if got := failingTests("ok  \tpkg\t0.1s\n"); len(got) != 0 {
		t.Errorf("failingTests = %v for a passing run, want none", got)
	}
}

// mustModule writes a throwaway Go module and returns its directory, so the
// checks below run against real `go test` exit statuses rather than a captured
// fixture of one. What they pin is the difference between two outcomes of a
// command, which a string cannot exercise.
func mustModule(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module mutatetest\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(dir, "m_test.go"), "package m\n\nimport \"testing\"\n\n"+body)
	return dir
}

const (
	selectedPasses = "func TestSelected(t *testing.T) {}\n"
	selectedFails  = "func TestSelected(t *testing.T) { t.Fatal(\"selected and unselected disagree\") }\n"
	omittedFails   = "func TestOmitted(t *testing.T) { t.Fatal(\"the mutation is caught here\") }\n"
	omittedPasses  = "func TestOmitted(t *testing.T) {}\n"
)

func TestWidenOnPass_NamesTheTestTheRunFilterLeavesOut(t *testing.T) {
	t.Parallel()

	root := mustModule(t, selectedPasses+omittedFails)
	got := widenOnPass(root, &spec{pkg: "./...", run: "TestSelected"}, mutation{name: "m"}, false)

	if got.verdict != excluded {
		t.Fatalf("verdict = %s, want EXCLUDED; a stale `run` line was reported as an inert assertion", got.verdict)
	}
	if !strings.Contains(got.evidence, "TestOmitted") {
		t.Errorf("evidence = %q, want it to name the excluded test", got.evidence)
	}
}

// The next several tests name fixture test functions — TestSelected,
// TestOmitted — that exist only as text inside a mustModule string literal (a
// throwaway module built at test time), never as a top-level declaration
// check_doc_citations' line-anchored scanner can see.
//
//doccite:ok TestSelected — mustModule fixture text (see selectedPasses/selectedFails), not a top-level declaration
//doccite:ok TestOmitted — mustModule fixture text (see omittedPasses/omittedFails), not a top-level declaration

func TestWidenOnPass_ReportsFlakyWhenTheSelectedTestIsWhatDisagrees(t *testing.T) {
	t.Parallel()

	// TestSelected is the only test in the package, and `run` selects it by
	// name — so a package-wide failure here cannot be `run` leaving anything
	// out. It is the same test disagreeing with itself between the two runs.
	root := mustModule(t, selectedFails)
	got := widenOnPass(root, &spec{pkg: "./...", run: "TestSelected"}, mutation{name: "m"}, false)

	if got.verdict != flaky {
		t.Fatalf("verdict = %s, want FLAKY; a selected test's own inconsistency was reported as a spec defect", got.verdict)
	}
	if !strings.Contains(got.evidence, "TestSelected") {
		t.Errorf("evidence = %q, want it to name the selected test", got.evidence)
	}
}

func TestWidenOnPass_MixedFailuresReportExcludedForTheOmittedName(t *testing.T) {
	t.Parallel()

	// Both TestSelected (selected by `run`) and TestOmitted (not) fail in the
	// package-wide run. TestOmitted's absence from `run` is a real spec gap
	// regardless of what else also failed, so the ruling is EXCLUDED, and the
	// evidence names only the test `run` is missing.
	root := mustModule(t, selectedFails+omittedFails)
	got := widenOnPass(root, &spec{pkg: "./...", run: "TestSelected"}, mutation{name: "m"}, false)

	if got.verdict != excluded {
		t.Fatalf("verdict = %s, want EXCLUDED; a mixed failure was reported as the selected test's own fault", got.verdict)
	}
	if !strings.Contains(got.evidence, "TestOmitted") {
		t.Errorf("evidence = %q, want it to name the excluded test", got.evidence)
	}
	if strings.Contains(got.evidence, "TestSelected") {
		t.Errorf("evidence = %q, want it to name only the excluded test, not the selected one too", got.evidence)
	}
}

func TestWidenOnPass_ReportsSurvivedWhenTheWholePackageIsGreen(t *testing.T) {
	t.Parallel()

	// Nothing in the package discriminates, which is a real SURVIVED. Claiming
	// an exclusion here would send the reader to edit a `run` line that is
	// already correct.
	root := mustModule(t, selectedPasses+omittedPasses)
	got := widenOnPass(root, &spec{pkg: "./...", run: "TestSelected"}, mutation{name: "m"}, false)

	if got.verdict != survived {
		t.Fatalf("verdict = %s, want SURVIVED", got.verdict)
	}
	if got.evidence != survivedEvidence {
		t.Errorf("evidence = %q, want %q", got.evidence, survivedEvidence)
	}
}

func TestWidenOnPass_SkipsTheWiderRunWhenTheSpecHasNoFilter(t *testing.T) {
	t.Parallel()

	// With no `run` line the command already ran the whole package, so there is
	// nothing wider to compare against.
	//
	// The root is a module whose package-wide run FAILS, which is what makes
	// this discriminate. An unusable directory would not: widening there dies
	// at the launch, and the launch-failure branch also returns SURVIVED — so
	// the assertion held whether the run was skipped or attempted and failed.
	// Here, a widening that happened at all reports EXCLUDED.
	root := mustModule(t, selectedPasses+omittedFails)
	got := widenOnPass(root, &spec{pkg: "./..."}, mutation{name: "m"}, false)
	if got.verdict != survived {
		t.Errorf("verdict = %s, want SURVIVED; the wider run was made despite the spec having no filter", got.verdict)
	}
}

func TestWidenOnPass_ReportsSurvivedWhenTheWiderRunCannotStart(t *testing.T) {
	t.Parallel()

	// A launch failure is not an observation of anything, so it cannot support
	// an exclusion. This is the branch the skip test above deliberately no
	// longer exercises, kept as its own case so both remain covered.
	got := widenOnPass(filepath.Join(t.TempDir(), "absent"), &spec{pkg: "./...", run: "TestSelected"}, mutation{name: "m"}, false)
	if got.verdict != survived {
		t.Errorf("verdict = %s, want SURVIVED", got.verdict)
	}
}

func excludedResult() []result {
	return []result{{
		name:     "m",
		verdict:  excluded,
		evidence: "`run` excludes TestOmitted, which kills this",
	}}
}

func flakyResult() []result {
	return []result{{
		name:     "m",
		verdict:  flaky,
		evidence: "TestSelected is selected by `run` and killed this mutation in the package-wide run but not in the filtered run — a determinism problem in the test, not the spec",
	}}
}

func TestConfirmExclusions_DowngradesFlakyWhenThePackageIsRedUnmutated(t *testing.T) {
	t.Parallel()

	// The same confirming run applies to a FLAKY claim as to an EXCLUDED one:
	// a package that is red even without the mutation was not made red by it,
	// so neither a spec defect nor a test's determinism is actually at issue.
	root := mustModule(t, selectedFails)
	got, err := confirmExclusions(root, &spec{pkg: "./...", run: "TestSelected"}, flakyResult())
	if err != nil {
		t.Fatalf("confirmExclusions: %v", err)
	}

	if got[0].verdict != survived {
		t.Fatalf("verdict = %s, want SURVIVED; an unrelated failure was reported as a flaky test", got[0].verdict)
	}
	if !strings.Contains(got[0].evidence, "red unmutated too") {
		t.Errorf("evidence = %q, want it to say the package was already red", got[0].evidence)
	}
}

func TestConfirmExclusions_KeepsFlakyWhenTheUnmutatedPackageIsGreen(t *testing.T) {
	t.Parallel()

	root := mustModule(t, selectedPasses)
	got, err := confirmExclusions(root, &spec{pkg: "./...", run: "TestSelected"}, flakyResult())
	if err != nil {
		t.Fatalf("confirmExclusions: %v", err)
	}

	if got[0].verdict != flaky {
		t.Errorf("verdict = %s, want FLAKY to stand when the package is green unmutated", got[0].verdict)
	}
}

func TestConfirmExclusions_DowngradesWhenThePackageIsRedUnmutated(t *testing.T) {
	t.Parallel()

	// widenOnPass observes only that the package is red WITH the mutation. A
	// package carrying an unrelated failure — a flake, a break in a file the
	// spec never names — is red either way, and the baseline cannot have caught
	// it, because the baseline runs only the filter.
	root := mustModule(t, selectedPasses+omittedFails)
	got, err := confirmExclusions(root, &spec{pkg: "./...", run: "TestSelected"}, excludedResult())
	if err != nil {
		t.Fatalf("confirmExclusions: %v", err)
	}

	if got[0].verdict != survived {
		t.Fatalf("verdict = %s, want SURVIVED; an unrelated failure was reported as a spec defect", got[0].verdict)
	}
	if !strings.Contains(got[0].evidence, "red unmutated too") {
		t.Errorf("evidence = %q, want it to say the package was already red", got[0].evidence)
	}
}

func TestConfirmExclusions_ErrorsWhenTheConfirmingRunCannotStart(t *testing.T) {
	t.Parallel()

	// A run that never starts observed nothing, so it is neither a red package
	// nor a green one. Downgrading on it would stamp every EXCLUDED row with
	// "the package is red unmutated too" — a sentence about a run that did not
	// happen, which is exactly the false green the verdict set exists to refuse.
	got, err := confirmExclusions(filepath.Join(t.TempDir(), "absent"), &spec{pkg: "./...", run: "TestSelected"}, excludedResult())
	if err == nil {
		t.Fatalf("confirmExclusions returned %v and no error for a run that could not launch", got)
	}
	if got != nil {
		t.Errorf("results = %v alongside an error, want nil", got)
	}
}

func TestConfirmExclusions_KeepsTheVerdictWhenTheUnmutatedPackageIsGreen(t *testing.T) {
	t.Parallel()

	root := mustModule(t, selectedPasses+omittedPasses)
	got, err := confirmExclusions(root, &spec{pkg: "./...", run: "TestSelected"}, excludedResult())
	if err != nil {
		t.Fatalf("confirmExclusions: %v", err)
	}

	if got[0].verdict != excluded {
		t.Errorf("verdict = %s, want EXCLUDED to stand when the package is green unmutated", got[0].verdict)
	}
}

func TestNeedsConfirmation_OnlyWhenSomethingClaimedAnExclusion(t *testing.T) {
	t.Parallel()

	// This is the predicate rather than the behaviour on purpose.
	// confirmExclusions returns the rows unchanged either way — it skipped the
	// run, or it made one and found no EXCLUDED row to downgrade — so a test
	// that asserts the verdicts are unchanged passes without the skip existing.
	killedRow := result{name: "a", verdict: killed, evidence: "x_test.go:1: boom"}
	survivedRow := result{name: "b", verdict: survived, evidence: survivedEvidence}

	if needsConfirmation([]result{killedRow, survivedRow}) {
		t.Error("a spec with no exclusion would pay for the confirming package-wide run")
	}
	if !needsConfirmation([]result{killedRow, survivedRow, excludedResult()[0]}) {
		t.Error("an EXCLUDED row would be reported without ever being confirmed")
	}
	if !needsConfirmation([]result{killedRow, survivedRow, flakyResult()[0]}) {
		t.Error("a FLAKY row would be reported without ever being confirmed")
	}
	if needsConfirmation(nil) {
		t.Error("an empty result set asked for a confirming run")
	}
}

func TestNote_TellsTheSpecDefectApartFromTheInertAssertion(t *testing.T) {
	t.Parallel()

	// SURVIVED points the reader at the test; EXCLUDED points them at the spec.
	// Sharing a note would undo the split the verdict exists to make.
	n := note(result{verdict: excluded})
	if !strings.Contains(n, "`run`") {
		t.Errorf("note(EXCLUDED) = %q, want it to name the run line", n)
	}
	if n == note(result{verdict: survived}) {
		t.Error("EXCLUDED and SURVIVED share a note; the verdicts are then only cosmetically distinct")
	}
}

func TestNote_TellsFlakyApartFromExcludedAndSurvived(t *testing.T) {
	t.Parallel()

	// FLAKY points the reader at the test's own determinism, not at `run` and
	// not at "the assertion never ran". A note that reused either of those
	// other two notes would send the reader to fix the wrong thing.
	n := note(result{verdict: flaky})
	if !strings.Contains(n, "determinism") {
		t.Errorf("note(FLAKY) = %q, want it to name the test's determinism", n)
	}
	if n == note(result{verdict: excluded}) {
		t.Error("FLAKY and EXCLUDED share a note; the verdicts are then only cosmetically distinct")
	}
	if n == note(result{verdict: survived}) {
		t.Error("FLAKY and SURVIVED share a note; the verdicts are then only cosmetically distinct")
	}
}
