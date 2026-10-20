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
	if err := j.AttachFiles(nil); err == nil {
		t.Error("AttachFiles(nil): nil error")
	}
	if j.Files() != nil {
		t.Fatal("Files() is non-nil before a successful AttachFiles")
	}
	ft := NewFileTable(twoFileSpecs())
	if err := j.AttachFiles(ft); err != nil {
		t.Fatalf("first AttachFiles: %v", err)
	}
	if j.Files() != ft {
		t.Error("Files() does not return the attached table")
	}
	if got := j.NumFiles(); got != 2 {
		t.Errorf("NumFiles = %d, want 2", got)
	}
	if err := j.AttachFiles(NewFileTable(twoFileSpecs())); err == nil {
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
	if err := j.AttachFiles(ft1); err != nil {
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

func restoreFixture(t *testing.T, counts ...int) (*Job, *Manifest) {
	t.Helper()
	specs := make([]FileSpec, 0, len(counts))
	var arts []JobArticle
	for fi, n := range counts {
		specs = append(specs, FileSpec{Subject: string(rune('a'+fi)) + ".bin", Bytes: int64(n) * 100, ArticleCount: n})
		for k := range n {
			arts = append(arts, JobArticle{ID: "<x@x>", Bytes: 100, Number: k + 1})
		}
	}
	m, err := NewManifestFromArticles(NewFileTable(specs), arts)
	if err != nil {
		t.Fatalf("NewManifestFromArticles: %v", err)
	}
	j := New("j", "j", Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j, m
}

// RestoreContent is the production hydration door: the manifest it receives
// was read from disk with a table of its own. A same-total manifest that
// splits the articles differently between files must be refused, or its
// articles would index against the wrong files.
func TestRestoreContent_RefusesASameCountDifferentSplit(t *testing.T) {
	j, _ := restoreFixture(t, 3, 1)
	j.Evict()
	_, other := restoreFixture(t, 1, 3)
	if err := j.RestoreContent(other); err == nil {
		t.Fatal("RestoreContent accepted a manifest that splits the same 4 articles differently")
	}
	if j.Resident() {
		t.Error("a refused RestoreContent left the job resident")
	}
}

func TestRestoreContent_AcceptsEqualSpecsFromAnotherTable(t *testing.T) {
	j, _ := restoreFixture(t, 3, 1)
	j.Evict()
	_, same := restoreFixture(t, 3, 1)
	if err := j.RestoreContent(same); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Files() != j.Files() {
		t.Error("the restored manifest is not indexed over the job's own table")
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
