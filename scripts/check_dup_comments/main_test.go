package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// block is a 5-line, >120-char comment block: over both default thresholds.
const block = `// This is a long explanatory comment block that exists only so the test
// has something over the minimum line count and the minimum character
// count at once. It says nothing useful about any particular declaration,
// which is exactly the property that makes a copy of it hard to notice
// when it is pasted above a second, unrelated function.
`

// writeGo materialises Go-ish files and returns their paths in a stable order.
func writeGo(t *testing.T, files map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, len(files))
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		paths = append(paths, p)
	}
	return paths
}

// scanAll runs the same collection main does, and returns the groups that
// would be reported. It FAILS on a marker error rather than ignoring one:
// main exits 2 on those before reporting anything, so a test that swallowed
// them would assert about a report main would never print.
func scanAll(t *testing.T, paths []string) map[string][]occurrence {
	t.Helper()
	groups := map[string][]occurrence{}
	for _, p := range paths {
		b, mErrs, err := scan(p, defaultMinLines, defaultMinChars)
		if err != nil {
			t.Fatalf("scan %s: %v", p, err)
		}
		if len(mErrs) > 0 {
			t.Fatalf("scan %s: %v", p, mErrs[0])
		}
		for k, v := range b {
			groups[k] = append(groups[k], v...)
		}
	}
	reported := map[string][]occurrence{}
	for k, occs := range groups {
		if len(occs) < 2 || sameBasename(occs) || allMarked(occs) {
			continue
		}
		reported[k] = occs
	}
	return reported
}

func TestScan_ReportsABlockDuplicatedAcrossFiles(t *testing.T) {
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + block + "func A() {}\n",
		"b/b.go": "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1", len(got))
	}
	for _, occs := range got {
		if len(occs) != 2 {
			t.Errorf("group has %d occurrences, want 2", len(occs))
		}
	}
}

// TestScan_ReportsABlockDuplicatedWithinOneFile is the shape sameBasename must
// NOT exempt: two copies in one file share a basename trivially, so a naive
// exemption would hide the commonest copy-paste of all.
func TestScan_ReportsABlockDuplicatedWithinOneFile(t *testing.T) {
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + block + "func A() {}\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1 (a within-file duplicate must not be exempt)", len(got))
	}
}

// TestScan_ExemptsPerPackageCopiesOfOneHelperFile pins the basename exemption:
// Go cannot share an unexported test helper across packages, so those copies
// are required rather than accidental.
func TestScan_ExemptsPerPackageCopiesOfOneHelperFile(t *testing.T) {
	got := scanAll(t, writeGo(t, map[string]string{
		"a/helpers_test.go": "package a\n\n" + block + "func A() {}\n",
		"b/helpers_test.go": "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 0 {
		t.Fatalf("reported %d groups, want 0 (same basename, distinct packages)", len(got))
	}
}

// TestScan_DifferentBasenamesAreNotExempt is the negative half of the test
// above. Without it, an exemption that always returned true would pass.
func TestScan_DifferentBasenamesAreNotExempt(t *testing.T) {
	got := scanAll(t, writeGo(t, map[string]string{
		"a/helpers_test.go": "package a\n\n" + block + "func A() {}\n",
		"b/other_test.go":   "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1", len(got))
	}
}

// TestScan_MarkerOnEveryCopyExemptsTheGroup pins that a group is exempt when
// EVERY copy carries a marker, and that the marker lines are stripped before
// normalisation — without the stripping the marked copies would hash
// differently from each other and the group would never form, so the exemption
// would appear to work for the wrong reason.
//
// This doc block previously named TestScan_MarkerOnOneCopyExemptsTheGroup, a
// function that no longer exists, and described one-copy exemption — the
// behaviour f7071c6a deliberately REMOVED. A stale doc asserting the opposite
// of the code, sitting inside the gate built to catch exactly that class.
func TestScan_MarkerOnEveryCopyExemptsTheGroup(t *testing.T) {
	marked := "// dupcomment:ok the two backends assert one shared contract\n//\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + marked + "func A() {}\n",
		"b/b.go": "package b\n\n" + marked + "func B() {}\n",
	}))
	if len(got) != 0 {
		t.Fatalf("reported %d groups, want 0 (every copy marked)", len(got))
	}
}

// TestScan_MarkerOnOneCopyDoesNotExemptTheGroup pins the fix for a real defect
// in the first version of this tool: the exemption was a repo-wide map from
// normalised text to reason, so one //dupcomment:ok anywhere whitelisted that
// text EVERYWHERE — including a copy pasted by accident later. The marker would
// then hide exactly what it exists to surface.
func TestScan_MarkerOnOneCopyDoesNotExemptTheGroup(t *testing.T) {
	marked := "// dupcomment:ok the two backends assert one shared contract\n//\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + marked + "func A() {}\n",
		"b/b.go": "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1 (an unmarked copy must still be reported)", len(got))
	}
}

// TestScan_BareMarkerDoesNotExempt pins that the reason is mandatory. An
// unexplained exemption is the thing the tool exists to surface.
//
// The bare marker is deliberately left UNTERMINATED while its partner is
// closed with a blank `//`. A bare marker cannot have a wrapped reason, so it
// is exempt from the termination rule, and this is what pins that exemption:
// terminating it here would make the test pass without exercising it.
func TestScan_BareMarkerDoesNotExempt(t *testing.T) {
	bare := "// dupcomment:ok\n" + block
	good := "// dupcomment:ok a real reason\n//\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + bare + "func A() {}\n",
		"b/b.go": "package b\n\n" + good + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1 (a marker with no reason must not suppress)", len(got))
	}
}

// TestScan_ShortAndSmallBlocksAreBelowThreshold pins both thresholds
// independently: a block under the line count and a block over the line count
// but under the character count must each be ignored.
func TestScan_ShortAndSmallBlocksAreBelowThreshold(t *testing.T) {
	twoLines := "// one line here that is quite long indeed and says a fair amount\n// two lines here that also runs on for a good while to pass chars\n"
	fiveTiny := "// a\n// b\n// c\n// d\n// e\n"
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + twoLines + "func A() {}\n\n" + fiveTiny + "func C() {}\n",
		"b/b.go": "package b\n\n" + twoLines + "func B() {}\n\n" + fiveTiny + "func D() {}\n",
	}))
	if len(got) != 0 {
		t.Fatalf("reported %d groups, want 0 (both blocks are below a threshold)", len(got))
	}
}

// TestScan_NormalisesWhitespace pins that reindentation does not hide a copy —
// the commonest way a pasted block differs from its original.
func TestScan_NormalisesWhitespace(t *testing.T) {
	var indented strings.Builder
	for _, l := range []string{
		"\t// This is a long explanatory comment block that exists only so the test",
		"\t//   has something over the minimum line count and the minimum character",
		"\t// count at once. It says   nothing useful about any particular declaration,",
		"\t// which is exactly the property that makes a copy of it hard to notice",
		"\t// when it is pasted above a second, unrelated function.",
	} {
		indented.WriteString(l + "\n")
	}
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + block + "func A() {}\n",
		"b/b.go": "package b\n\nfunc B() {\n" + indented.String() + "\tx := 1\n\t_ = x\n}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1 (reindentation must not hide a copy)", len(got))
	}
}

// TestScan_MarkerPlacementDoesNotAffectGrouping pins that a marker followed by
// a blank comment line — the conventional way to write one — still hashes equal
// to the unmarked twin.
//
// Without it the marked copy carries an extra empty entry in its key, the group
// never forms, and the finding vanishes. That is suppression by accident rather
// than by exemption, and it is the second time this tool grew that bug: once
// via an unmatched bare marker, once via the blank line after a matched one.
func TestScan_MarkerPlacementDoesNotAffectGrouping(t *testing.T) {
	spaced := "// dupcomment:ok a real reason\n//\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + spaced + "func A() {}\n",
		"b/b.go": "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1; a blank line after the marker must "+
			"not stop the marked copy grouping with its unmarked twin", len(got))
	}
}

// TestScan_MultiLineReasonDoesNotAffectGrouping pins that the marker owns its
// whole paragraph. A one-line reason is the exception, not the rule, and if the
// continuation lines stayed in the key the marked copy would stop matching its
// twin — suppressing the finding by accident instead of exempting it.
func TestScan_MultiLineReasonDoesNotAffectGrouping(t *testing.T) {
	long := "// dupcomment:ok the queue and queue_test packages each need their\n" +
		"// own copy; Go cannot share an unexported helper across that boundary.\n//\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + long + "func A() {}\n",
		"b/b.go": "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1; a multi-line reason must not stop "+
			"the marked copy grouping with its unmarked twin", len(got))
	}
}

// scanMarkerErrs runs scan over paths and returns only the marker errors, for
// the cases where those ARE the subject rather than an obstacle.
func scanMarkerErrs(t *testing.T, paths []string) []error {
	t.Helper()
	var out []error
	for _, p := range paths {
		_, mErrs, err := scan(p, defaultMinLines, defaultMinChars)
		if err != nil {
			t.Fatalf("scan %s: %v", p, err)
		}
		out = append(out, mErrs...)
	}
	return out
}

// TestScan_UnterminatedReasonIsRefused pins the fix for a silent suppression
// that reproduced against this tool.
//
// A marker whose reason wraps onto a second line with no closing `//` leaves
// that second line in the block's identity. The marked copy then hashes
// differently from its unmarked twin, the group falls below two occurrences,
// and NEITHER is reported — so adding a marker to one copy silences the other,
// the exact inversion of what the marker means.
//
// It cannot be fixed by a rule, only refused: nothing in the text says whether
// the line after the marker continues the reason or resumes the comment.
// Consuming to the end of the block erases the text being identified;
// consuming only the marker's line leaves the wrapped reason in the key.
//
// The assertion is on the ERROR, not on the report. Before the fix this
// fixture produced an empty report and exit 0 — a passing run — which is why
// asserting "no group is reported" would pin the bug rather than the fix.
func TestScan_UnterminatedReasonIsRefused(t *testing.T) {
	unterminated := "// dupcomment:ok deliberate, because the two subsystems\n" +
		"// document one protocol and must not drift apart.\n" + block
	paths := writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + unterminated + "func A() {}\n",
		"b/b.go": "package b\n\n" + block + "func B() {}\n",
	})

	errs := scanMarkerErrs(t, paths)
	if len(errs) != 1 {
		t.Fatalf("got %d marker errors, want 1 — an unterminated reason is being accepted, "+
			"which makes the marked copy hash differently and silences its unmarked twin", len(errs))
	}
	if !errors.Is(errs[0], errUnterminatedMarker) {
		t.Errorf("err = %v, want one wrapping errUnterminatedMarker", errs[0])
	}
}

// TestScan_MarkerAsTheLastLineNeedsNoTermination pins the other side of the
// same rule.
//
// A marker on the block's final line has nothing after it, so no line is
// ambiguous and requiring a trailing blank `//` would be noise. This is also
// where markers most often sit, so getting it wrong would make the rule
// unusable rather than strict.
func TestScan_MarkerAsTheLastLineNeedsNoTermination(t *testing.T) {
	trailing := block + "// dupcomment:ok the two backends assert one shared contract\n"
	paths := writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + trailing + "func A() {}\n",
		"b/b.go": "package b\n\n" + trailing + "func B() {}\n",
	})
	if errs := scanMarkerErrs(t, paths); len(errs) != 0 {
		t.Fatalf("got %d marker errors, want 0: %v", len(errs), errs)
	}
	if got := scanAll(t, paths); len(got) != 0 {
		t.Errorf("reported %d groups, want 0 — a trailing marker on every copy must still exempt", len(got))
	}
}

// TestScan_ProseNamingTheMarkerIsNotAMarker pins the anchor on markerRe.
//
// An unanchored match turns any sentence that NAMES the marker into one. Real
// examples were live in the tree: several in this tool's own file, and one in
// internal/postproc, each of the form "a //dupcomment:ok marker was added
// to...". Every block containing such a sentence was silently exempt, with the
// rest of the sentence captured as its reason.
//
// The fixture puts the sentence in BOTH copies, which is what makes the
// assertion discriminate. Read as prose, the lines are part of the identity
// and the pair is reported. Read as a marker, they are stripped, both copies
// carry a reason, allMarked exempts the group, and nothing is reported —
// which is precisely the silent exemption this anchor removes.
func TestScan_ProseNamingTheMarkerIsNotAMarker(t *testing.T) {
	// The mention must carry TEXT AFTER it on the same line, and be closed by
	// a blank `//`. Both details are what make the fixture discriminate: an
	// unanchored match needs following text to capture a non-empty reason, and
	// the blank line is what lets the (mis)read marker be a valid terminated
	// one rather than failing as an unterminated one for a different reason.
	prose := "// The gate fires here because a //dupcomment:ok marker with a reason\n" +
		"// was added to both copies of the helper below.\n//\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + prose + "func A() {}\n",
		"b/b.go": "package b\n\n" + prose + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1 — prose that merely names the marker is being "+
			"treated as one, so any block mentioning it is silently exempt", len(got))
	}
}
