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
		run      string
		want     []string
		anchored bool
	}{
		{"TestA|TestB", []string{"TestA", "TestB"}, false},
		{"^(TestA|TestB|TestC)$", []string{"TestA", "TestB", "TestC"}, true},
		{"TestOnlyOne", []string{"TestOnlyOne"}, false},
	}
	for _, tc := range cases {
		got, anchored, ok := plainAlternation(tc.run)
		if !ok {
			t.Errorf("plainAlternation(%q) ok = false, want true", tc.run)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("plainAlternation(%q) = %v, want %v", tc.run, got, tc.want)
		}
		if anchored != tc.anchored {
			t.Errorf("plainAlternation(%q) anchored = %v, want %v", tc.run, anchored, tc.anchored)
		}
	}
}

func TestPlainAlternation_FallsBackForAnythingThatIsNotABareNameList(t *testing.T) {
	t.Parallel()

	// Each of these must fall back to the baseline's existing ranNothing
	// check rather than being split: an empty run, a subtest path (go test
	// -list never reports subtests), a regexp carrying metacharacters other
	// than the `|` alternation this check understands, and a single name
	// anchored the ^…$ way rather than the ^(…)$ way this check strips.
	for _, run := range []string{"", "TestFoo/subcase", "TestFoo.*", "^TestFoo$", "TestFoo|TestBar.*"} {
		if _, _, ok := plainAlternation(run); ok {
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
