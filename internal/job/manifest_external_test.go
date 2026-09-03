package job_test

import (
	"encoding/json"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// files is the shape an NZB-parsing caller outside this package builds. It is
// declared here rather than inside each test so that the two tests below
// construct a Manifest from literally the same input, which is what makes the
// second one's comparison meaningful.
func externalFiles() []job.JobFile {
	return []job.JobFile{
		{
			Subject: "big.rar",
			Bytes:   300,
			Articles: []job.JobArticle{
				{ID: "a1@x", Bytes: 100, Number: 1},
				{ID: "a2@x", Bytes: 200, Number: 2},
			},
		},
		{
			Subject:        "big.vol000+01.par2",
			Bytes:          50,
			IsPar2Recovery: true,
			Articles:       []job.JobArticle{{ID: "p1@x", Bytes: 50, Number: 1}},
		},
	}
}

// TestNewManifest_IsReachableFromOutsideThePackage is the whole point of
// exporting the constructor: an NZB-parsing caller in another package can
// build a Manifest without a JSON round-trip. It asserts the derived state —
// the offsets prefix sum, the totals and the recovery figures — because those
// are what a caller that hand-rolled the struct would get wrong, and they are
// unreachable from outside except through this door.
func TestNewManifest_IsReachableFromOutsideThePackage(t *testing.T) {
	m := job.NewManifest(externalFiles())

	if got, want := m.NumFiles(), 2; got != want {
		t.Errorf("NumFiles = %d, want %d", got, want)
	}
	if got, want := m.NumArticles(), 3; got != want {
		t.Errorf("NumArticles = %d, want %d", got, want)
	}
	if got, want := m.TotalBytes(), int64(350); got != want {
		t.Errorf("TotalBytes = %d, want %d", got, want)
	}
	if got, want := m.RecoveryBytes(), int64(50); got != want {
		t.Errorf("RecoveryBytes = %d, want %d", got, want)
	}
	if got, want := m.RecoveryFiles(), 1; got != want {
		t.Errorf("RecoveryFiles = %d, want %d", got, want)
	}
	lo, hi := m.FileRange(1)
	if lo != 2 || hi != 3 {
		t.Errorf("FileRange(1) = (%d,%d), want (2,3)", lo, hi)
	}
	if got := m.ArticleID(2); got != "p1@x" {
		t.Errorf("ArticleID(2) = %q, want %q", got, "p1@x")
	}
}

// TestNewManifest_AgreesWithTheJSONRoundTrip pins the reason exporting this is
// not a second constructor: UnmarshalJSON reaches the same function, so the
// direct door and the persisted one cannot report different derived state.
// Comparing every exported figure is what would catch a future UnmarshalJSON
// that filled fields itself instead of delegating.
func TestNewManifest_AgreesWithTheJSONRoundTrip(t *testing.T) {
	direct := job.NewManifest(externalFiles())

	blob, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var loaded job.Manifest
	if err := json.Unmarshal(blob, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if direct.NumFiles() != loaded.NumFiles() ||
		direct.NumArticles() != loaded.NumArticles() ||
		direct.TotalBytes() != loaded.TotalBytes() ||
		direct.RecoveryBytes() != loaded.RecoveryBytes() ||
		direct.RecoveryFiles() != loaded.RecoveryFiles() {
		t.Fatalf("round trip diverged: direct(files=%d arts=%d total=%d recBytes=%d recFiles=%d) "+
			"loaded(files=%d arts=%d total=%d recBytes=%d recFiles=%d)",
			direct.NumFiles(), direct.NumArticles(), direct.TotalBytes(),
			direct.RecoveryBytes(), direct.RecoveryFiles(),
			loaded.NumFiles(), loaded.NumArticles(), loaded.TotalBytes(),
			loaded.RecoveryBytes(), loaded.RecoveryFiles())
	}
	for i := range direct.NumArticles() {
		if direct.ArticleID(i) != loaded.ArticleID(i) ||
			direct.ArticleBytes(i) != loaded.ArticleBytes(i) ||
			direct.ArticleNumber(i) != loaded.ArticleNumber(i) {
			t.Errorf("article %d diverged after round trip", i)
		}
	}
}
