package job

import "testing"

func seedSpecs() []FileSpec {
	return []FileSpec{
		{Subject: "a.bin", Bytes: 1000, ArticleCount: 4},
		{Subject: "b.bin", Bytes: 301, ArticleCount: 3},
		{Subject: "c.bin", Bytes: 50, ArticleCount: 1},
	}
}

func doneCount(b bitset) int {
	n := 0
	for i := range b.Len() {
		if b.Get(i) {
			n++
		}
	}
	return n
}

// Durability §3: a restore seed sets no article bit.
func TestAttachFiles_SeedSetsNoDoneBit(t *testing.T) {
	ft := NewFileTable(seedSpecs())
	j := New("j", "j", Policy{})
	seed := []FileSeed{{Recorded: 4}, {Recorded: 2}, {Recorded: 1, Complete: true}}
	if err := j.AttachFiles(ft, seed); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	p := j.progress
	if n := doneCount(p.done); n != 0 {
		t.Errorf("done.Count() = %d after a seed, want 0", n)
	}
	if p.pendingArticles != ft.TotalArticles() {
		t.Errorf("pendingArticles = %d, want %d", p.pendingArticles, ft.TotalArticles())
	}
	for fi := range seedSpecs() {
		if j.progress.FileVerified(fi) {
			t.Errorf("FileVerified(%d) = true after a seed", fi)
		}
	}
	if j.AllVerified() {
		t.Error("AllVerified() = true after a seed")
	}
	if !p.files[2].Complete {
		t.Error("seeded Complete was not applied")
	}
}

func TestAttachFiles_SeededNonFetchedFileOwesNoPending(t *testing.T) {
	ft := NewFileTable(seedSpecs())
	j := New("j", "j", Policy{})
	seed := []FileSeed{{}, {Fetch: FetchNever}, {}}
	if err := j.AttachFiles(ft, seed); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	if want := ft.TotalArticles() - 3; j.progress.pendingArticles != want {
		t.Errorf("pendingArticles = %d, want %d", j.progress.pendingArticles, want)
	}
}

func TestAttachFiles_NilSeedIsVerified(t *testing.T) {
	j := New("j", "j", Policy{})
	if err := j.AttachFiles(NewFileTable(seedSpecs()), nil); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	if !j.AllVerified() {
		t.Error("AllVerified() = false for a nil seed")
	}
	for fi := range j.progress.files {
		if j.progress.files[fi].Recorded != 0 {
			t.Errorf("file %d Recorded = %d, want 0", fi, j.progress.files[fi].Recorded)
		}
	}
}

func TestAttachFiles_RefusesASeedOfTheWrongLength(t *testing.T) {
	j := New("j", "j", Policy{})
	if err := j.AttachFiles(NewFileTable(seedSpecs()), make([]FileSeed, 2)); err == nil {
		t.Fatal("AttachFiles accepted a seed of 2 for a table of 3 files")
	}
	if j.Files() != nil || j.HasProgress() {
		t.Error("a refused seed left something attached")
	}
}

// R2: no files means nothing is unverified.
func TestAttachFiles_ZeroFilesIsVerified(t *testing.T) {
	for name, seed := range map[string][]FileSeed{"nil": nil, "empty": {}} {
		j := New("j", "j", Policy{})
		if err := j.AttachFiles(NewFileTable(nil), seed); err != nil {
			t.Fatalf("%s: AttachFiles: %v", name, err)
		}
		if !j.AllVerified() {
			t.Errorf("%s seed: AllVerified() = false for a zero-file job", name)
		}
	}
	if New("j", "j", Policy{}).AllVerified() {
		t.Error("AllVerified() = true with no progress attached")
	}
}

func TestFileBytesDownloaded_EstimateIsBoundedAndInBytesUnits(t *testing.T) {
	j := New("j", "j", Policy{})
	seed := []FileSeed{{Recorded: 0}, {Recorded: 2}, {Recorded: 1}}
	if err := j.AttachFiles(NewFileTable(seedSpecs()), seed); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	p := j.progress
	if got := p.FileBytesDownloaded(0); got != 0 {
		t.Errorf("Recorded 0: %d, want 0", got)
	}
	if got, want := p.FileBytesDownloaded(1), int64(301*2/3); got != want {
		t.Errorf("Recorded 2 of 3: %d, want %d", got, want)
	}
	if got := p.FileBytesDownloaded(2); got != 50 {
		t.Errorf("Recorded n of n: %d, want 50", got)
	}
	// Verified: the maintained figure, not the estimate. Test-only flip.
	p.files[1].Verified = true
	p.files[1].BytesDownloaded = 7
	if got := p.FileBytesDownloaded(1); got != 7 {
		t.Errorf("after Verified: %d, want 7", got)
	}
}

func TestInstallFileVerification_MarksVerifiedAndZeroesRecorded(t *testing.T) {
	ft := NewFileTable(seedSpecs())
	m, err := NewManifestFromArticles(ft, seedArticles())
	if err != nil {
		t.Fatalf("NewManifestFromArticles: %v", err)
	}
	j := New("j", "j", Policy{})
	if err := j.AttachFiles(ft, []FileSeed{{Recorded: 3}, {Recorded: 1}, {}}); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	if err := j.AttachArticles(m); err != nil {
		t.Fatalf("AttachArticles: %v", err)
	}
	if _, err := j.InstallFileVerification(FileVerification{FileIdx: 0}); err != nil {
		t.Fatalf("InstallFileVerification: %v", err)
	}
	if !j.progress.FileVerified(0) || j.progress.files[0].Recorded != 0 {
		t.Errorf("file 0: Verified=%v Recorded=%d, want true, 0", j.progress.FileVerified(0), j.progress.files[0].Recorded)
	}
	if j.progress.FileVerified(1) || j.progress.files[1].Recorded != 1 {
		t.Error("installing file 0 touched file 1")
	}
}

func seedArticles() []JobArticle {
	var arts []JobArticle
	for _, s := range seedSpecs() {
		for n := 1; n <= s.ArticleCount; n++ {
			arts = append(arts, JobArticle{ID: "<x@x>", Bytes: int(s.Bytes) / s.ArticleCount, Number: n})
		}
	}
	return arts
}

func TestRemainingBytes_UsesTheEstimateWhileUnverified(t *testing.T) {
	j := New("j", "j", Policy{})
	seed := []FileSeed{{Recorded: 1}, {Recorded: 2}, {Recorded: 0}}
	if err := j.AttachFiles(NewFileTable(seedSpecs()), seed); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	want := int64(1000-1000*1/4) + int64(301-301*2/3) + int64(50)
	if got := j.progress.RemainingBytes(); got != want {
		t.Errorf("RemainingBytes = %d, want %d", got, want)
	}
}
