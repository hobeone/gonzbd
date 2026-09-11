package main

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The section check is worth only as much as its matcher. A regex that matched
// NOTHING would report zero findings against a clean tree and be
// indistinguishable from one that works — which is how a gate ships inert. So
// these pin both directions: the forms that must be recognised, and the ones
// that must resolve once recognised.

func TestSectionRE_RecognisesTheFormsTheTreeUses(t *testing.T) {
	cases := []struct {
		name, line, wantDoc, wantSec string
	}{
		{
			name:    "backticked path, bare section",
			line:    "the rule lives in `docs/go-standards.md` § Concurrency & Locking today",
			wantDoc: "docs/go-standards.md", wantSec: "Concurrency & Locking",
		},
		{
			name:    "bare path, quoted section",
			line:    `see docs/commit-cycle.md § "The red check" for the measurement`,
			wantDoc: "docs/commit-cycle.md", wantSec: "The red check",
		},
		{
			name:    "backticked path, quoted section",
			line:    "`AGENTS.md` § \"Step 4 in practice\" gives the rules.",
			wantDoc: "AGENTS.md", wantSec: "Step 4 in practice",
		},
		{
			name:    "fully-qualified numeric section",
			line:    "see docs/durability-contract.md §6 for the gap that left",
			wantDoc: "docs/durability-contract.md", wantSec: "6",
		},
		{
			name:    "numeric section with a subsection",
			line:    "maintained in `docs/post_processing_spec.md` § 6.7, which is where",
			wantDoc: "docs/post_processing_spec.md", wantSec: "6.7",
		},
		{
			name:    "no space after the section mark",
			line:    "`docs/ARCHITECTURE.md` §Post-Processing covers it",
			wantDoc: "docs/ARCHITECTURE.md", wantSec: "Post-Processing",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := sectionRE.FindStringSubmatch(c.line)
			if m == nil {
				t.Fatalf("sectionRE matched nothing in %q", c.line)
			}
			sec := cmp.Or(m[2], m[3], m[4])
			if m[1] != c.wantDoc {
				t.Errorf("doc = %q, want %q", m[1], c.wantDoc)
			}
			// An unquoted citation legitimately runs into the sentence around
			// it, so the contract is that the captured span STARTS with the
			// section name — not that it equals it.
			if !strings.HasPrefix(sec, c.wantSec) {
				t.Errorf("section = %q, want a span starting with %q", sec, c.wantSec)
			}
		})
	}
}

func TestHeadingWords_DropsMarkupAndSectionNumbers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"### 1. Concurrency & Locking", []string{"concurrency", "&", "locking"}},
		{"## Ground rules", []string{"ground", "rules"}},
		{"#### 10.4 API Modes Reference", []string{"api", "modes", "reference"}},
		{"## The red check — why it must be observed", []string{"the", "red", "check", "—", "why", "it", "must", "be", "observed"}},
		{"### 5.B Response identity", []string{"response", "identity"}},
		{"## `Cross` is the only door", []string{"cross", "is", "the", "only", "door"}},
		// A heading that continues past its name must still match a citation
		// of the name alone; without the per-word trim this is "tick:".
		{"## The tick: a ticker owns liveness", []string{"the", "tick", "a", "ticker", "owns", "liveness"}},
		// Lowercase tails are real: docs/TESTING.md has "## 3a. Crash-Consistency
		// Tests" and docs/durability-contract.md has "### 9a. ...". Rejecting
		// them left the number in the list, so a citation of the NAME could
		// never match.
		{"## 3a. Crash-Consistency Tests", []string{"crash-consistency", "tests"}},
		{"### 9a. Only storage conditions reach `Stallable`", []string{"only", "storage", "conditions", "reach", "stallable"}},
	}
	for _, c := range cases {
		if got := headingWords(c.in); !slices.Equal(got, c.want) {
			t.Errorf("headingWords(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSectionResolves(t *testing.T) {
	root := t.TempDir()
	target := "docs/target.md"
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Title\n\n## 1. Concurrency & Locking\n\ntext\n\n" +
		"## The red check — why it must be observed\n\ntext\n\n" +
		"# Overview\n\ntext\n\n## 6. The record is authoritative\n\ntext\n\n" +
		"### 10.4 API Modes Reference\n\ntext\n\n" +
		// Several contracts number their invariants as a top-level ordered
		// list rather than as headings, and cite them as "§5" all the same.
		"## Mandatory invariants\n\n5. **Emitted-is-transient** contract\n"
	if err := os.WriteFile(filepath.Join(root, target), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	headingCache = map[string][]heading{}

	cases := []struct {
		name, cited string
		want        bool
	}{
		{"exact heading", "Concurrency & Locking", true},
		{"citation is a prefix of a longer heading", "The red check", true},
		{"citation runs into surrounding prose", "The red check and then some prose", true},
		{"heading does not exist", "Coordination Architecture", false},
		{"right document, wrong section entirely", "Truncate Bound", false},
		{"first word matches, second does not", "The truncate bound", false},
		// A one-word heading was skipped outright: an unquoted citation
		// captures trailing prose, so len(want) >= 2 while len(h) == 1, and
		// the old `len(h) < n` guard discarded the heading that matched.
		{"one-word heading, citation carries trailing prose", "Overview and then some prose", true},
		{"one-word heading, cited alone", "Overview", true},
		{"numeric citation matching a numbered heading", "6", true},
		{"numeric citation with a subsection", "10.4", true},
		{"numeric citation naming no heading", "42", false},
		{"numeric citation naming a top-level ordered list item", "5", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sectionResolves(root, "docs/other.md", target, c.cited)
			if got != c.want {
				t.Errorf("sectionResolves(%q) = %v, want %v", c.cited, got, c.want)
			}
		})
	}
}

// A missing document is pathRE's finding, not this check's. Reporting both
// would print two lines for one defect and make the section check look noisy
// on exactly the change that deletes a doc.
func TestSectionResolves_DefersToThePathCheckWhenTheDocIsGone(t *testing.T) {
	root := t.TempDir()
	headingCache = map[string][]heading{}
	if !sectionResolves(root, "docs/other.md", "docs/absent.md", "Anything At All") {
		t.Error("a section citation into a MISSING document was reported; pathRE already reports the document")
	}
}
