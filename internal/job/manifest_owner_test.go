package job

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A Manifest has one owner for its derived state: newManifest, reached through
// NewManifestFromArticles. UnmarshalJSON delegates to it rather than filling
// the fields itself, so a round-trip cannot produce a Manifest that differs
// from one built directly — which is what lets the rest of the system reason
// about "a manifest" without asking which constructor made it.
func TestManifestRoundTrip_MatchesDirectConstruction(t *testing.T) {
	t.Parallel()
	files := []JobFile{
		{
			Subject: "a.bin",
			Bytes:   300,
			Articles: []JobArticle{
				{ID: "a1@h", Bytes: 100, Number: 1},
				{ID: "a2@h", Bytes: 200, Number: 2},
			},
		},
		{
			Subject:        "a.vol01+02.par2",
			Bytes:          50,
			IsPar2Recovery: true,
			Articles:       []JobArticle{{ID: "p1@h", Bytes: 50, Number: 1}},
		},
	}

	direct := NewManifest(files)
	blob, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(blob, []byte(`"date"`)) {
		t.Errorf("marshalled manifest carries a date key: %s", blob)
	}
	var loaded Manifest
	if err := json.Unmarshal(blob, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got, want := loaded.TotalBytes(), direct.TotalBytes(); got != want {
		t.Errorf("TotalBytes = %d, want %d", got, want)
	}
	if got, want := loaded.NumFiles(), direct.NumFiles(); got != want {
		t.Errorf("NumFiles = %d, want %d", got, want)
	}
	if got, want := loaded.NumArticles(), direct.NumArticles(); got != want {
		t.Errorf("NumArticles = %d, want %d", got, want)
	}
	if got, want := loaded.RecoveryBytes(), direct.RecoveryBytes(); got != want {
		t.Errorf("RecoveryBytes = %d, want %d", got, want)
	}
	if got, want := loaded.RecoveryFiles(), direct.RecoveryFiles(); got != want {
		t.Errorf("RecoveryFiles = %d, want %d", got, want)
	}
	for i := range direct.NumArticles() {
		if got, want := loaded.ArticleID(i), direct.ArticleID(i); got != want {
			t.Errorf("ArticleID(%d) = %q, want %q", i, got, want)
		}
		if got, want := loaded.ArticleBytes(i), direct.ArticleBytes(i); got != want {
			t.Errorf("ArticleBytes(%d) = %d, want %d", i, got, want)
		}
		if got, want := loaded.ArticleNumber(i), direct.ArticleNumber(i); got != want {
			t.Errorf("ArticleNumber(%d) = %d, want %d", i, got, want)
		}
	}
	for fi := range direct.NumFiles() {
		if got, want := loaded.Files().Bytes(fi), direct.Files().Bytes(fi); got != want {
			t.Errorf("Files().Bytes(%d) = %d, want %d", fi, got, want)
		}
		if got, want := loaded.Files().Subject(fi), direct.Files().Subject(fi); got != want {
			t.Errorf("Files().Subject(%d) = %q, want %q", fi, got, want)
		}
		lo, hi := loaded.FileRange(fi)
		wlo, whi := direct.FileRange(fi)
		if lo != wlo || hi != whi {
			t.Errorf("FileRange(%d) = [%d,%d), want [%d,%d)", fi, lo, hi, wlo, whi)
		}
	}
}

// NewManifestFromArticles is the one constructor, and it refuses a flat article
// list whose length the table does not account for. The error names both
// counts so a reader can tell which side is short.
func TestNewManifestFromArticles_RefusesAnArticleCountTheTableDoesNotHave(t *testing.T) {
	t.Parallel()
	ft := NewFileTable([]FileSpec{
		{Subject: "a.bin", Bytes: 200, ArticleCount: 2},
		{Subject: "b.bin", Bytes: 100, ArticleCount: 1},
	})
	m, err := NewManifestFromArticles(ft, []JobArticle{
		{ID: "a1@h", Bytes: 100, Number: 1}, {ID: "a2@h", Bytes: 100, Number: 2},
	})
	if err == nil {
		t.Fatalf("NewManifestFromArticles = %v, nil; want an error for 2 articles against a 3-article table", m)
	}
	for _, want := range []string{"3", "2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the count %s", err, want)
		}
	}
}

// No total is persisted. A stored copy of a value recomputable from the file
// list is a second source of truth, and the one that can drift; the blob
// carries only the files, and a stray total_bytes key in one is read by no one.
func TestManifestDoesNotPersistTotalBytes(t *testing.T) {
	t.Parallel()
	m := NewManifest([]JobFile{{Subject: "a.bin", Bytes: 300, Articles: []JobArticle{
		{ID: "a1@h", Bytes: 100, Number: 1}, {ID: "a2@h", Bytes: 200, Number: 2},
	}}})

	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(blob, []byte("total_bytes")) {
		t.Errorf("marshalled manifest carries a total_bytes key: %s", blob)
	}

	var loaded Manifest
	if err := json.Unmarshal(blob, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := loaded.TotalBytes(); got != 300 {
		t.Errorf("TotalBytes after round trip = %d, want 300 derived from the file list", got)
	}
}

// A Message-ID read back from disk is re-checked, and a manifest carrying one
// that cannot be requested is refused whole.
//
// This is the one place internal/queue does not trust its own earlier output,
// and the reason is specific: internal/nntp interpolates the ID into a
// command line and no longer validates it, so a CR or LF is a command
// injection. A manifest written before that rule existed at parse time can
// still hold one, and the no-backwards-compatibility rule covers persistence
// FORMAT rather than a security invariant.
//
// Refusing the whole manifest rather than dropping the article is deliberate:
// the caller treats the error as a corrupt manifest and fails the job
// visibly, where a silent drop would leave a job quietly short of articles.
func TestManifestRefusesAnUnfetchableMessageIDFromDisk(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, id string }{
		{"carriage return", "a\r\nDATA@h"},
		{"line feed", "a\nb@h"},
		{"space", "a b@h"},
		{"interior bracket", "a>b@h"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := json.Marshal(manifestJSON{Files: []manifestJSONFile{{
				Subject:  "a.bin",
				Bytes:    100,
				Articles: []manifestJSONArticle{{ID: tc.id, Bytes: 100, Number: 1}},
			}}})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var m Manifest
			if err := json.Unmarshal(blob, &m); err == nil {
				t.Fatalf("Unmarshal(%q) = nil, want an error — the ID would reach "+
					"the wire unchecked", tc.id)
			}
		})
	}
}

// A well-formed manifest must still load, or the check above would refuse
// every job on restart.
func TestManifestAcceptsOrdinaryMessageIDsFromDisk(t *testing.T) {
	t.Parallel()
	blob, err := json.Marshal(manifestJSON{Files: []manifestJSONFile{{
		Subject:  "a.bin",
		Bytes:    100,
		Articles: []manifestJSONArticle{{ID: "plain@example.com", Bytes: 100, Number: 1}},
	}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := m.ArticleID(0); got != "plain@example.com" {
		t.Errorf("ArticleID(0) = %q, want it loaded verbatim", got)
	}
}
