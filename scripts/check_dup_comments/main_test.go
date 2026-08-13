package main

import (
	"os"
	"path/filepath"
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
	var paths []string
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
// would be reported.
func scanAll(t *testing.T, paths []string) map[string][]occurrence {
	t.Helper()
	groups := map[string][]occurrence{}
	marked := map[string]string{}
	for _, p := range paths {
		b, m, err := scan(p, 4, 120)
		if err != nil {
			t.Fatalf("scan %s: %v", p, err)
		}
		for k, v := range b {
			groups[k] = append(groups[k], v...)
		}
		for k, r := range m {
			marked[k] = r
		}
	}
	reported := map[string][]occurrence{}
	for k, occs := range groups {
		if len(occs) < 2 || sameBasename(occs) || marked[k] != "" {
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

// TestScan_MarkerOnOneCopyExemptsTheGroup pins that the marker line is stripped
// before normalisation. If it were not, the marked copy would hash differently
// and the group would never match in the first place — the exemption would
// appear to work for the wrong reason.
func TestScan_MarkerOnOneCopyExemptsTheGroup(t *testing.T) {
	marked := "// dupcomment:ok the two backends assert one shared contract\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + marked + "func A() {}\n",
		"b/b.go": "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 0 {
		t.Fatalf("reported %d groups, want 0 (marker on one copy exempts the group)", len(got))
	}
}

// TestScan_BareMarkerDoesNotExempt pins that the reason is mandatory. An
// unexplained exemption is the thing the tool exists to surface.
func TestScan_BareMarkerDoesNotExempt(t *testing.T) {
	bare := "// dupcomment:ok\n" + block
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + bare + "func A() {}\n",
		"b/b.go": "package b\n\n" + block + "func B() {}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1 (a marker with no reason must not suppress)", len(got))
	}
}

// TestScan_ShortAndSmallBlocksAreBelowThreshold pins both thresholds
// independently: a block under the line count and a block over the line count
// but under the character count must each be ignored.
func TestScan_ShortAndSmallBlocksAreBelowThreshold(t *testing.T) {
	threeLines := "// one line here\n// two lines here\n// three lines here now\n"
	fiveTiny := "// a\n// b\n// c\n// d\n// e\n"
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + threeLines + "func A() {}\n\n" + fiveTiny + "func C() {}\n",
		"b/b.go": "package b\n\n" + threeLines + "func B() {}\n\n" + fiveTiny + "func D() {}\n",
	}))
	if len(got) != 0 {
		t.Fatalf("reported %d groups, want 0 (both blocks are below a threshold)", len(got))
	}
}

// TestScan_NormalisesWhitespace pins that reindentation does not hide a copy —
// the commonest way a pasted block differs from its original.
func TestScan_NormalisesWhitespace(t *testing.T) {
	indented := ""
	for _, l := range []string{
		"\t// This is a long explanatory comment block that exists only so the test",
		"\t//   has something over the minimum line count and the minimum character",
		"\t// count at once. It says   nothing useful about any particular declaration,",
		"\t// which is exactly the property that makes a copy of it hard to notice",
		"\t// when it is pasted above a second, unrelated function.",
	} {
		indented += l + "\n"
	}
	got := scanAll(t, writeGo(t, map[string]string{
		"a/a.go": "package a\n\n" + block + "func A() {}\n",
		"b/b.go": "package b\n\nfunc B() {\n" + indented + "\tx := 1\n\t_ = x\n}\n",
	}))
	if len(got) != 1 {
		t.Fatalf("reported %d groups, want 1 (reindentation must not hide a copy)", len(got))
	}
}
