package job

import "testing"

func twoFileSpecs() []FileSpec {
	return []FileSpec{
		{Subject: "a.bin", Bytes: 200, ArticleCount: 2},
		{Subject: "b.bin", Bytes: 100, ArticleCount: 1},
	}
}

func twoFileArticles() []JobArticle {
	return []JobArticle{
		{ID: "<a0@x>", Bytes: 100, Number: 1},
		{ID: "<a1@x>", Bytes: 100, Number: 2},
		{ID: "<b0@x>", Bytes: 100, Number: 1},
	}
}

// AttachFiles installs the file table and the progress record derived from it;
// a second call would discard a live record, so it refuses.
func TestAttachFiles_RefusesASecondAttach(t *testing.T) {
	j := New("j", "j", Policy{})
	if err := j.AttachFiles(nil, nil); err == nil {
		t.Error("AttachFiles(nil): nil error")
	}
	if j.Files() != nil {
		t.Fatal("Files() is non-nil before a successful AttachFiles")
	}
	ft := NewFileTable(twoFileSpecs())
	if err := j.AttachFiles(ft, nil); err != nil {
		t.Fatalf("first AttachFiles: %v", err)
	}
	if j.Files() != ft {
		t.Error("Files() does not return the attached table")
	}
	if got := j.NumFiles(); got != 2 {
		t.Errorf("NumFiles = %d, want 2", got)
	}
	if err := j.AttachFiles(NewFileTable(twoFileSpecs()), nil); err == nil {
		t.Error("second AttachFiles returned nil; it must refuse rather than replace a live record")
	}
}

// AttachArticles refuses a manifest built from a different table, even one
// with identical specs: the pairing is by identity, not by shape.
func TestAttachArticles_RefusesAManifestFromAnotherTable(t *testing.T) {
	ft1 := NewFileTable(twoFileSpecs())
	ft2 := NewFileTable(twoFileSpecs())
	m2, err := NewManifestFromArticles(ft2, twoFileArticles())
	if err != nil {
		t.Fatalf("NewManifestFromArticles: %v", err)
	}
	j := New("j", "j", Policy{})
	if err := j.AttachFiles(ft1, nil); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	if err := j.AttachArticles(m2); err == nil {
		t.Error("AttachArticles accepted a manifest built from another FileTable")
	}
	if j.Resident() {
		t.Error("a refused AttachArticles left the job resident")
	}
}

func TestAttachArticles_RequiresFiles(t *testing.T) {
	m, err := NewManifestFromArticles(NewFileTable(twoFileSpecs()), twoFileArticles())
	if err != nil {
		t.Fatalf("NewManifestFromArticles: %v", err)
	}
	j := New("j", "j", Policy{})
	if err := j.AttachArticles(m); err == nil {
		t.Error("AttachArticles before AttachFiles: nil error")
	}
	if err := j.AttachArticles(nil); err == nil {
		t.Error("AttachArticles(nil): nil error")
	}
}

// Re-attaching the same manifest after Evict recomputes the counters in place
// from the bits written while evicted, and refuses a second attach while the
// manifest is still resident.
func TestAttachArticles_RecomputesInPlace(t *testing.T) {
	j, m := evictedThreeArticleJob(t)
	if err := j.MarkArticleFailed(2, WholeArticle); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if err := j.AttachArticles(m); err != nil {
		t.Fatalf("AttachArticles: %v", err)
	}
	p := j.Progress()
	if got := p.ArticlesFailed(); got != 1 {
		t.Errorf("ArticlesFailed = %d, want 1", got)
	}
	if got := j.FailedBytes(); got != 100 {
		t.Errorf("FailedBytes = %d, want 100", got)
	}
	if err := j.AttachArticles(m); err == nil {
		t.Error("AttachArticles on a resident job: nil error")
	}
}

// AttachFiles is the only writer of Job.recoveryBytes, so a job reports its
// recovery volumes' size exactly when a table carrying them is attached, and
// reports 0 before then.
func TestAttachFiles_SetsRecoveryBytesFromTheTable(t *testing.T) {
	j := New("j", "j", Policy{})
	if got := j.RecoveryBytes(); got != 0 {
		t.Fatalf("RecoveryBytes() = %d before an attach, want 0", got)
	}
	ft := NewFileTable([]FileSpec{
		{Subject: "a.rar", Bytes: 200, ArticleCount: 2},
		{Subject: "a.vol01+01.par2", Bytes: 70, ArticleCount: 1},
	})
	if err := j.AttachFiles(ft, nil); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	if got, want := j.RecoveryBytes(), int64(70); got != want {
		t.Errorf("RecoveryBytes() = %d after AttachFiles, want %d (the table's recovery volumes)", got, want)
	}
}
