package main

import (
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
			sec := m[2]
			if sec == "" {
				sec = m[3]
			}
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
	body := "# Title\n\n## 1. Concurrency & Locking\n\ntext\n\n## The red check — why it must be observed\n\ntext\n"
	if err := os.WriteFile(filepath.Join(root, target), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	headingCache = map[string][][]string{}

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
	headingCache = map[string][][]string{}
	if !sectionResolves(root, "docs/other.md", "docs/absent.md", "Anything At All") {
		t.Error("a section citation into a MISSING document was reported; pathRE already reports the document")
	}
}
