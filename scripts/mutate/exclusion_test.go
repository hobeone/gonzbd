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
	// nothing wider to compare against. An unusable root is what shows none was
	// attempted: a launch there fails, and the verdict is SURVIVED regardless.
	got := widenOnPass(filepath.Join(t.TempDir(), "absent"), &spec{pkg: "./..."}, mutation{name: "m"}, false)
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

func TestConfirmExclusions_DowngradesWhenThePackageIsRedUnmutated(t *testing.T) {
	t.Parallel()

	// widenOnPass observes only that the package is red WITH the mutation. A
	// package carrying an unrelated failure — a flake, a break in a file the
	// spec never names — is red either way, and the baseline cannot have caught
	// it, because the baseline runs only the filter.
	root := mustModule(t, selectedPasses+omittedFails)
	got := confirmExclusions(root, &spec{pkg: "./...", run: "TestSelected"}, excludedResult())

	if got[0].verdict != survived {
		t.Fatalf("verdict = %s, want SURVIVED; an unrelated failure was reported as a spec defect", got[0].verdict)
	}
	if !strings.Contains(got[0].evidence, "red unmutated too") {
		t.Errorf("evidence = %q, want it to say the package was already red", got[0].evidence)
	}
}

func TestConfirmExclusions_KeepsTheVerdictWhenTheUnmutatedPackageIsGreen(t *testing.T) {
	t.Parallel()

	root := mustModule(t, selectedPasses+omittedPasses)
	got := confirmExclusions(root, &spec{pkg: "./...", run: "TestSelected"}, excludedResult())

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
