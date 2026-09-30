package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. It is not safe to run in parallel with anything else that
// writes to os.Stdout, since the swap is global — callers must not mark
// their test t.Parallel().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	os.Stdout = saved
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String()
}

func TestPlainAlternation_SplitsAndStripsCapturingAnchoring(t *testing.T) {
	t.Parallel()

	cases := []struct {
		run           string
		want          []string
		startAnchored bool
		endAnchored   bool
	}{
		{"TestA|TestB", []string{"TestA", "TestB"}, false, false},
		{"^(TestA|TestB|TestC)$", []string{"TestA", "TestB", "TestC"}, true, true},
		{"TestOnlyOne", []string{"TestOnlyOne"}, false, false},
		// The shared-prefix shape issue #633 added: the group does not span
		// the whole line, so the prefix is prepended to each alternative.
		{"Test(A|B)$", []string{"TestA", "TestB"}, false, true},
		{"^Test(A|B)$", []string{"TestA", "TestB"}, true, true},
	}
	for _, tc := range cases {
		got, startAnchored, endAnchored, ok := plainAlternation(tc.run)
		if !ok {
			t.Errorf("plainAlternation(%q) ok = false, want true", tc.run)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("plainAlternation(%q) = %v, want %v", tc.run, got, tc.want)
		}
		if startAnchored != tc.startAnchored || endAnchored != tc.endAnchored {
			t.Errorf("plainAlternation(%q) startAnchored,endAnchored = %v,%v, want %v,%v",
				tc.run, startAnchored, endAnchored, tc.startAnchored, tc.endAnchored)
		}
	}
}

func TestPlainAlternation_FallsBackForAnythingThatIsNotABareNameList(t *testing.T) {
	t.Parallel()

	// Each of these must fall back to the baseline's existing ranNothing
	// check rather than being split: an empty run, a subtest path (go test
	// -list never reports subtests), a regexp carrying metacharacters other
	// than the `|` alternation this check understands, a single name
	// anchored the ^…$ way rather than a `(`…`)$` group, a leading `^` with
	// no group at all, a prefixed group whose expansion is not a valid test
	// name, and a group missing its closing `)$`.
	for _, run := range []string{
		"", "TestFoo/subcase", "TestFoo.*", "^TestFoo$", "TestFoo|TestBar.*",
		"^TestFoo", "Test.(A|B)$", "Test(A|B",
	} {
		if _, _, _, ok := plainAlternation(run); ok {
			t.Errorf("plainAlternation(%q) ok = true, want false", run)
		}
	}
}

func TestListTests_ListsOnlyTopLevelTestFunctions(t *testing.T) {
	t.Parallel()

	root := mustModule(t,
		"func TestOne(t *testing.T) {}\n"+
			"func TestTwo(t *testing.T) {}\n"+
			"func BenchmarkX(b *testing.B) {}\n"+
			"func Example() {}\n")

	got, err := listTests(root, &spec{pkg: "./..."})
	if err != nil {
		t.Fatalf("listTests: %v", err)
	}
	slices.Sort(got)
	want := []string{"TestOne", "TestTwo"}
	if !slices.Equal(got, want) {
		t.Errorf("listTests = %v, want %v (Benchmark/Example must be excluded)", got, want)
	}
}

func TestDeadRunFilterNames_NamesTheAlternativeThatMatchesNoTest(t *testing.T) {
	t.Parallel()

	root := mustModule(t, selectedPasses+omittedPasses) // declares TestSelected and TestOmitted
	sp := &spec{pkg: "./...", run: "TestSelected|TestDoesNotExist"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if !slices.Equal(got, []string{"TestDoesNotExist"}) {
		t.Errorf("deadRunFilterNames = %v, want [TestDoesNotExist]", got)
	}
}

func TestDeadRunFilterNames_ReportsNothingWhenEveryAlternativeExists(t *testing.T) {
	t.Parallel()

	root := mustModule(t, selectedPasses+omittedPasses)
	sp := &spec{pkg: "./...", run: "TestSelected|TestOmitted"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if got != nil {
		t.Errorf("deadRunFilterNames = %v, want nil", got)
	}
}

// The next several tests name fixture test functions — TestSelected,
// TestWrapTestSelected, TestSelectedFoo — that exist only as text inside a
// mustModule string literal (a throwaway module built at test time), never as
// a top-level declaration check_doc_citations' line-anchored scanner can see.
//
//doccite:ok TestSelected — mustModule fixture text (see selectedPasses), not a top-level declaration
//doccite:ok TestWrapTestSelected — mustModule fixture text below, not a top-level declaration
//doccite:ok TestSelectedFoo — mustModule fixture text below, not a top-level declaration

func TestDeadRunFilterNames_PrefixedGroupNamesEachAlternativeLive(t *testing.T) {
	t.Parallel()

	// The shape issue #633 added: the shared "Test" prefix sits outside the
	// group, as it does in the six specs the issue found.
	root := mustModule(t, selectedPasses+omittedPasses) // declares TestSelected and TestOmitted
	sp := &spec{pkg: "./...", run: "Test(Selected|Omitted)$"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if got != nil {
		t.Errorf("deadRunFilterNames = %v, want nil", got)
	}
}

func TestDeadRunFilterNames_PrefixedGroupNamesTheDeadAlternative(t *testing.T) {
	t.Parallel()

	root := mustModule(t, selectedPasses+omittedPasses)
	sp := &spec{pkg: "./...", run: "Test(Selected|DoesNotExist)$"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if !slices.Equal(got, []string{"TestDoesNotExist"}) {
		t.Errorf("deadRunFilterNames = %v, want [TestDoesNotExist]", got)
	}
}

func TestDeadRunFilterNames_PrefixedGroupWithoutCaretMatchesALongerTestNameBySuffix(t *testing.T) {
	t.Parallel()

	// "Test(Selected)$" has no leading ^, so it only end-anchors: the
	// expanded alternative "TestSelected" is live because
	// TestWrapTestSelected ends with it, even though no test is named
	// exactly "TestSelected".
	root := mustModule(t, "func TestWrapTestSelected(t *testing.T) {}\n")
	sp := &spec{pkg: "./...", run: "Test(Selected)$"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if got != nil {
		t.Errorf("deadRunFilterNames = %v, want nil: an end-anchored-only match is a suffix, not equality", got)
	}
}

func TestDeadRunFilterNames_PrefixedGroupEndAnchorRejectsAPrefixOfALongerName(t *testing.T) {
	t.Parallel()

	// "Test(Selected)$" end-anchors: TestSelectedFoo has "TestSelected" as a
	// prefix of its name, not a suffix, so it must NOT count as a match —
	// unlike the bare, unanchored form where containment anywhere is enough.
	root := mustModule(t, "func TestSelectedFoo(t *testing.T) {}\n")
	sp := &spec{pkg: "./...", run: "Test(Selected)$"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if !slices.Equal(got, []string{"TestSelected"}) {
		t.Errorf("deadRunFilterNames = %v, want [TestSelected]: $ requires the match to reach the end", got)
	}
}

func TestDeadRunFilterNames_LeadingCaretOnAPrefixedGroupRequiresExactEquality(t *testing.T) {
	t.Parallel()

	// Same package as above, but the leading ^ anchors the start too:
	// TestWrapTestSelected no longer counts, since it is not literally equal
	// to "TestSelected".
	root := mustModule(t, "func TestWrapTestSelected(t *testing.T) {}\n")
	sp := &spec{pkg: "./...", run: "^Test(Selected)$"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if !slices.Equal(got, []string{"TestSelected"}) {
		t.Errorf("deadRunFilterNames = %v, want [TestSelected]: leading ^ requires exact equality", got)
	}
}

func TestDeadRunFilterNames_AcceptsAnUnanchoredPrefixOfARealTest(t *testing.T) {
	t.Parallel()

	// go test -run matches unanchored, so a prefix of a declared test's name
	// selects that test and is a live alternative, not a dead one.
	root := mustModule(t, selectedPasses+omittedPasses)
	sp := &spec{pkg: "./...", run: "TestSel|TestOmitted"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if got != nil {
		t.Errorf("deadRunFilterNames = %v, want nil: an unanchored prefix selects a real test", got)
	}
}

func TestDeadRunFilterNames_AnchoredPrefixSelectsNothing(t *testing.T) {
	t.Parallel()

	// Wrapped in ^(…)$, TestSel must equal a test name, and none is.
	root := mustModule(t, selectedPasses+omittedPasses)
	sp := &spec{pkg: "./...", run: "^(TestSel|TestOmitted)$"}

	got, err := deadRunFilterNames(root, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v", err)
	}
	if !slices.Equal(got, []string{"TestSel"}) {
		t.Errorf("deadRunFilterNames = %v, want [TestSel]", got)
	}
}

func TestDeadRunFilterNames_SkipsANonPlainAlternationRunWithoutListingTests(t *testing.T) {
	t.Parallel()

	// A root that cannot possibly be listed against — if this fell through to
	// listTests, the call would fail and this would return a non-nil error
	// instead of (nil, nil). That is the pin: the non-plain `run` line below
	// must never reach `go test -list` at all.
	unusable := filepath.Join(t.TempDir(), "does-not-exist")
	sp := &spec{pkg: "./...", run: "TestFoo.*"}

	got, err := deadRunFilterNames(unusable, sp)
	if err != nil {
		t.Fatalf("deadRunFilterNames: %v (want it to skip listTests entirely)", err)
	}
	if got != nil {
		t.Errorf("deadRunFilterNames = %v, want nil", got)
	}
}

func TestFilterMatchesName_UnanchoredSubstringLikeGoTest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		run, name string
		want      bool
	}{
		{"TestSelected", "TestSelected", true},
		{"TestSelected", "TestOmitted", false},
		// go test -run matches unanchored, so a prefix of a declared test's
		// name still selects it.
		{"TestSel", "TestSelected", true},
		{"TestSelected|TestOther", "TestOther", true},
		{"TestSelected|TestOther", "TestOmitted", false},
		{"^TestSelected$", "TestSelected", true},
		{"^TestSelected$", "TestSelectedFoo", false},
		// Only the first slash-separated segment governs a top-level name.
		{"TestSelected/subcase", "TestSelected", true},
	}
	for _, tc := range cases {
		if got := filterMatchesName(tc.run, tc.name); got != tc.want {
			t.Errorf("filterMatchesName(%q, %q) = %v, want %v", tc.run, tc.name, got, tc.want)
		}
	}
}

func TestFilterMatchesName_InvalidRegexpIsNonSelecting(t *testing.T) {
	t.Parallel()

	// "(" is not a fragment go test's own flag parsing would have accepted
	// either; this must report false rather than panicking.
	if filterMatchesName("Test(", "TestSelected") {
		t.Error("filterMatchesName with an invalid regexp fragment = true, want false")
	}
}

func TestReportRunFilter_NamesTheDeadAlternativeAndReturnsNonZero(t *testing.T) {
	var code int
	out := captureStdout(t, func() {
		code = reportRunFilter("./p/", []string{"TestGhost"})
	})
	if code != 1 {
		t.Errorf("reportRunFilter = %d, want 1", code)
	}
	if !strings.Contains(out, "TestGhost") || !strings.Contains(out, string(runFilter)) {
		t.Errorf("reportRunFilter output = %q, want it to name RUNFILTER and TestGhost", out)
	}
}
