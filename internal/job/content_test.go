package job

import "testing"

// TestAttachContent_IsTheSoleConstructorOfThePair pins Rule 2's owner model
// for the content tier: nothing may produce a (Manifest, JobProgress) pair
// except AttachContent, so the two can never describe different jobs.
//
// newManifest and Manifest.UnmarshalJSON populating the same fields by two
// paths is the defect this shape exists to prevent — they had already
// diverged over totalBytes (see the spec's Rule 2, "Two constructors for one
// type").
func TestAttachContent_IsTheSoleConstructorOfThePair(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))

	if j.Resident() {
		t.Fatal("a fresh Job must not be resident")
	}
	if _, err := j.Manifest(); err == nil {
		t.Fatal("Manifest() on a non-resident job must error, not return nil,nil")
	}

	m := newManifest([]JobFile{{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}}})
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
}

func TestJob_NumFiles(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))
	if got := j.NumFiles(); got != 0 {
		t.Fatalf("NumFiles before attach = %d, want 0", got)
	}

	m := newManifest([]JobFile{
		{Subject: "file1.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "file2.rar", Bytes: 200, Articles: []JobArticle{{ID: "<2@x>", Bytes: 200}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if got := j.NumFiles(); got != 2 {
		t.Fatalf("NumFiles after attach = %d, want 2", got)
	}

	j.Evict()
	if j.Resident() {
		t.Fatal("expected non-resident after Evict")
	}
	if got := j.NumFiles(); got != 2 {
		t.Fatalf("NumFiles after Evict = %d, want 2 (progress remains resident)", got)
	}
}

