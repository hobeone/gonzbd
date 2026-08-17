package queue

import (
	"encoding/json"
	"testing"
	"time"
)

// A Manifest has one owner for its derived state: newManifest. UnmarshalJSON
// delegates to it rather than filling the fields itself, so a round-trip
// cannot produce a Manifest that differs from one built directly — which is
// what lets the rest of the system reason about "a manifest" without asking
// which constructor made it.
func TestManifestRoundTripMatchesDirectConstruction(t *testing.T) {
	files := []JobFile{
		{
			Subject: "a.bin",
			Date:    time.Unix(1700000000, 0).UTC(),
			Bytes:   300,
			Articles: []JobArticle{
				{ID: "a1@h", Bytes: 100, Number: 1},
				{ID: "a2@h", Bytes: 200, Number: 2},
			},
		},
		{
			Subject:        "a.vol01+02.par2",
			Date:           time.Unix(1700000001, 0).UTC(),
			Bytes:          50,
			IsPar2Recovery: true,
			Articles:       []JobArticle{{ID: "p1@h", Bytes: 50, Number: 1}},
		},
	}

	direct := newManifest(files)
	blob, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
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
	for i := range direct.NumArticles() {
		if got, want := loaded.ArticleID(i), direct.ArticleID(i); got != want {
			t.Errorf("ArticleID(%d) = %q, want %q", i, got, want)
		}
	}
	for fi := range direct.NumFiles() {
		if got, want := loaded.FileBytes(fi), direct.FileBytes(fi); got != want {
			t.Errorf("FileBytes(%d) = %d, want %d", fi, got, want)
		}
	}
}

// total_bytes is written but not read. A blob whose stored total disagrees
// with its own file list must load with the DERIVED figure, not the stored
// one — otherwise the persisted copy is a second source of truth and the one
// that can drift.
func TestManifestDerivesTotalBytesRatherThanTrustingTheBlob(t *testing.T) {
	blob := []byte(`{"files":[{"subject":"a.bin","bytes":300,"articles":[` +
		`{"id":"a1@h","bytes":100,"number":1},{"id":"a2@h","bytes":200,"number":2}]}],` +
		`"total_bytes":999999}`)

	var m Manifest
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := m.TotalBytes(); got != 300 {
		t.Errorf("TotalBytes = %d, want 300 derived from the file list, not the "+
			"stored 999999", got)
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
