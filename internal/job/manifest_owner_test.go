package job

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A Manifest has one owner for its derived state: newManifest, reached through
// NewManifestFromArticles. The file holds the articles alone, so a round trip
// is Marshal, DecodeArticles, then NewManifestFromArticles over the table the
// caller already has: the result cannot differ from a Manifest built directly.
func TestDecodeArticles_RoundTripsMarshalJSON(t *testing.T) {
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
			Subject:  "a.vol01+02.par2",
			Bytes:    50,
			Articles: []JobArticle{{ID: "p1@h", Bytes: 50, Number: 1}},
		},
	}

	direct := NewManifest(files)
	blob, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(blob, []byte(`"articles"`)) {
		t.Errorf("marshalled manifest has no articles key: %s", blob)
	}
	for _, key := range []string{`"subject"`, `"files"`, `"date"`, `"is_par2_recovery"`, `"total_bytes"`} {
		if bytes.Contains(blob, []byte(key)) {
			t.Errorf("marshalled manifest carries a %s key, which the job_files record owns: %s", key, blob)
		}
	}
	arts, err := DecodeArticles(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("DecodeArticles: %v", err)
	}
	loaded, err := NewManifestFromArticles(direct.Files(), arts)
	if err != nil {
		t.Fatalf("NewManifestFromArticles: %v", err)
	}

	if got, want := loaded.NumArticles(), direct.NumArticles(); got != want {
		t.Fatalf("NumArticles = %d, want %d", got, want)
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
	if loaded.Files() != direct.Files() {
		t.Error("the loaded manifest is not indexed over the table it was given")
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

// A Message-ID read back from disk is re-checked, and an article list carrying
// one that cannot be requested is refused whole.
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
func TestDecodeArticles_RefusesAnUnfetchableMessageID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, id string }{
		{"carriage return", "a\r\nDATA@h"},
		{"line feed", "a\nb@h"},
		{"space", "a b@h"},
		{"interior bracket", "a>b@h"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := json.Marshal(manifestJSON{Articles: []manifestJSONArticle{
				{ID: "ok@h", Bytes: 100, Number: 1},
				{ID: tc.id, Bytes: 100, Number: 2},
			}})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			arts, err := DecodeArticles(bytes.NewReader(blob))
			if err == nil {
				t.Fatalf("DecodeArticles(%q) = %v, nil; want an error: the ID would reach the wire unchecked", tc.id, arts)
			}
			if arts != nil {
				t.Errorf("DecodeArticles returned %d articles alongside its error; the whole list is refused", len(arts))
			}
		})
	}
}

// A well-formed article list must still load, or the check above would refuse
// every job on restart.
func TestDecodeArticles_AcceptsOrdinaryMessageIDs(t *testing.T) {
	t.Parallel()
	blob, err := json.Marshal(manifestJSON{Articles: []manifestJSONArticle{{ID: "plain@example.com", Bytes: 100, Number: 1}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	arts, err := DecodeArticles(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("DecodeArticles: %v", err)
	}
	if len(arts) != 1 || arts[0].ID != "plain@example.com" {
		t.Errorf("DecodeArticles = %v, want the one article loaded verbatim", arts)
	}
}
