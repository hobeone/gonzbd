package job

import (
	"errors"
	"testing"
)

// TestAttachContent_IsTheSoleConstructorOfThePair pins Rule 2's owner model
// for the content tier: nothing may produce a (Manifest, JobProgress) pair
// except AttachContent, so the two can never describe different jobs.
//
// NewManifest and Manifest.UnmarshalJSON populating the same fields by two
// paths is the defect this shape exists to prevent — they had already
// diverged over totalBytes (see AGENTS.md Rule 2, "Two constructors for one
// type").
func TestAttachContent_IsTheSoleConstructorOfThePair(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))

	if j.Resident() {
		t.Fatal("a fresh Job must not be resident")
	}
	if _, err := j.Manifest(); err == nil {
		t.Fatal("Manifest() on a non-resident job must error, not return nil,nil")
	} else if !errors.Is(err, ErrNotResident) {
		t.Fatalf("Manifest() error = %v, want ErrNotResident", err)
	}

	m := NewManifest([]JobFile{{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if !j.Resident() {
		t.Fatal("Resident() must be true after AttachContent")
	}
	got, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest after attach: %v", err)
	}
	if got.NumArticles() != 1 {
		t.Fatalf("NumArticles = %d, want 1", got.NumArticles())
	}
	if j.Progress() == nil {
		t.Fatal("Progress() must be non-nil once content is attached")
	}
	if j.Progress().PendingArticles() != 1 {
		t.Fatalf("PendingArticles = %d, want 1", j.Progress().PendingArticles())
	}
	// The five scalars are written by the same two writers as the pair, so a
	// manifest installed without them is not a state this type can reach.
	if j.TotalBytes() != 100 || j.NumFiles() != 1 || j.NumArticles() != 1 {
		t.Fatalf("scalars = (%d, %d, %d), want (100, 1, 1)",
			j.TotalBytes(), j.NumFiles(), j.NumArticles())
	}
}

// TestEvict_KeepsProgressAndTheScalars pins the tier split
// docs/queue-lifecycle.md defines: the manifest is the evictable tier, and
// progress plus the five derived scalars are the always-resident one.
//
// Without this, an Evict that cleared progress too would pass every assertion
// in the constructor test above and silently make the queue listing and the
// abort checks unanswerable for any job that is not currently running.
func TestEvict_KeepsProgressAndTheScalars(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))
	m := NewManifest([]JobFile{{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	j.Evict()

	if j.Resident() {
		t.Fatal("Resident() must be false after Evict")
	}
	if _, err := j.Manifest(); !errors.Is(err, ErrNotResident) {
		t.Fatalf("Manifest() after Evict: err = %v, want ErrNotResident", err)
	}
	if j.Progress() == nil {
		t.Fatal("Evict must not drop the always-resident progress record")
	}
	if j.TotalBytes() != 100 {
		t.Fatalf("TotalBytes() after Evict = %d, want 100", j.TotalBytes())
	}
}

// TestArticleDoors_RefuseANonResidentJob pins that every mutator reports
// ErrNotResident rather than silently skipping the job — #261's silent-skip
// class, in its new home.
func TestArticleDoors_RefuseANonResidentJob(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))

	doors := map[string]func() error{
		"MarkArticleDone":     func() error { return j.MarkArticleDone(0) },
		"MarkArticleFailed":   func() error { return j.MarkArticleFailed(0) },
		"MarkArticleEmitted":  func() error { return j.MarkArticleEmitted(0) },
		"ClearArticleEmitted": func() error { return j.ClearArticleEmitted(0) },
		"MarkFileComplete":    func() error { return j.MarkFileComplete(0) },
	}
	for name, call := range doors {
		if err := call(); !errors.Is(err, ErrNotResident) {
			t.Errorf("%s on a non-resident job: err = %v, want ErrNotResident", name, err)
		}
	}
}

// TestArticleDoors_AccountForBytesAndFiles pins that the doors reach the
// progress record rather than merely returning nil, and that an out-of-range
// index is a reported refusal rather than a silent no-op.
func TestArticleDoors_AccountForBytesAndFiles(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))
	m := NewManifest([]JobFile{{
		Subject:  "a.rar",
		Bytes:    300,
		Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}, {ID: "<2@x>", Bytes: 200}},
	}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if err := j.MarkArticleDone(0); err != nil {
		t.Fatalf("MarkArticleDone: %v", err)
	}
	if !j.Progress().ArticleDone(0) {
		t.Error("MarkArticleDone did not reach the progress record")
	}
	if err := j.MarkArticleFailed(1); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if !j.Progress().ArticleFailed(1) {
		t.Error("MarkArticleFailed did not reach the progress record")
	}
	if got := j.Progress().FailedBytes(); got != 200 {
		t.Errorf("FailedBytes = %d, want 200 — the door must charge the failure "+
			"the manifest's byte count for that article", got)
	}

	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	if err := j.ClearArticleEmitted(0); err != nil {
		t.Fatalf("ClearArticleEmitted: %v", err)
	}

	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if !j.Progress().FileComplete(0) {
		t.Error("MarkFileComplete did not reach the progress record")
	}

	for name, call := range map[string]func() error{
		"MarkArticleDone":     func() error { return j.MarkArticleDone(2) },
		"MarkArticleFailed":   func() error { return j.MarkArticleFailed(2) },
		"MarkArticleEmitted":  func() error { return j.MarkArticleEmitted(2) },
		"ClearArticleEmitted": func() error { return j.ClearArticleEmitted(2) },
		"MarkFileComplete":    func() error { return j.MarkFileComplete(1) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s with an out-of-range index returned nil, want a refusal", name)
		}
	}
}

// TestRestoreContent_RefusesAMismatchedPair pins the one check that makes
// RestoreContent a second WRITER rather than a second CONSTRUCTOR: it installs
// progress it did not derive, so it must prove the two halves describe the same
// job before the pair can be observed.
func TestRestoreContent_RefusesAMismatchedPair(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))
	m := NewManifest([]JobFile{{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}}})
	other := NewManifest([]JobFile{
		{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "b.rar", Bytes: 100, Articles: []JobArticle{{ID: "<2@x>", Bytes: 100}}},
	})

	if err := j.RestoreContent(m, newJobProgress(other)); err == nil {
		t.Fatal("RestoreContent accepted progress describing a different manifest")
	}
	if j.Resident() {
		t.Fatal("a refused RestoreContent must install nothing")
	}
	if err := j.RestoreContent(nil, newJobProgress(m)); err == nil {
		t.Error("RestoreContent accepted a nil manifest")
	}
	if err := j.RestoreContent(m, nil); err == nil {
		t.Error("RestoreContent accepted nil progress")
	}

	if err := j.RestoreContent(m, newJobProgress(m)); err != nil {
		t.Fatalf("RestoreContent on a matching pair: %v", err)
	}
	if !j.Resident() || j.TotalBytes() != 100 {
		t.Fatalf("RestoreContent left the job resident=%v totalBytes=%d, want true/100",
			j.Resident(), j.TotalBytes())
	}
}
