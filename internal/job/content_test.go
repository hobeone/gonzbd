package job

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

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

func TestJob_ContentMethods(t *testing.T) {
	j := New("job-1", "test-job", PolicyFromPP(3))
	m := newManifest([]JobFile{
		{
			Subject: "file1.rar",
			Bytes:   200,
			Articles: []JobArticle{
				{ID: "<1@x>", Bytes: 100, Number: 1},
				{ID: "<2@x>", Bytes: 100, Number: 2},
			},
		},
		{
			Subject:        "file2.vol01+01.par2",
			Bytes:          100,
			IsPar2Recovery: true,
			Deferred:       true,
			Articles: []JobArticle{
				{ID: "<3@x>", Bytes: 100, Number: 3},
			},
		},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}

	// DeferredRecoveryIndices
	if diff := j.DeferredRecoveryIndices(); len(diff) != 1 || diff[0] != 1 {
		t.Fatalf("DeferredRecoveryIndices = %v, want [1]", diff)
	}

	// CountUnfinishedArticles
	count, err := j.CountUnfinishedArticles(0)
	if err != nil || count != 2 {
		t.Fatalf("CountUnfinishedArticles = %d, err = %v, want 2, nil", count, err)
	}

	// SetFileFilename
	if err := j.SetFileFilename(0, "renamed.rar"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	if got := j.Progress().FileFilename(0); got != "renamed.rar" {
		t.Fatalf("FileFilename = %q, want renamed.rar", got)
	}

	// AckDurable
	inv, nArt, err := j.AckDurable([]int32{0, 99})
	if err != nil || inv != 1 || nArt != 3 {
		t.Fatalf("AckDurable = inv:%d, nArt:%d, err:%v; want inv:1, nArt:3, nil", inv, nArt, err)
	}
	if count, _ := j.CountUnfinishedArticles(0); count != 1 {
		t.Fatalf("CountUnfinishedArticles after AckDurable = %d, want 1", count)
	}

	// SetFileCRC32FromRuns
	runs := []durability.Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: 200, CRC32: 0x12345678},
	}
	ok, err := j.SetFileCRC32FromRuns(0, runs)
	if err != nil || !ok {
		t.Fatalf("SetFileCRC32FromRuns = %v, %v, want true, nil", ok, err)
	}
	if got := j.Progress().FileAssembledCRC32(0); got != 0x12345678 {
		t.Fatalf("FileAssembledCRC32 = %x, want 0x12345678", got)
	}

	// SeedFromRuns
	if err := j.SeedFromRuns(runs); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if count, _ := j.CountUnfinishedArticles(0); count != 0 {
		t.Fatalf("CountUnfinishedArticles after SeedFromRuns = %d, want 0", count)
	}

	// ReplaceFromRuns
	if err := j.ReplaceFromRuns([]int32{0}, nil); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}
	if count, _ := j.CountUnfinishedArticles(0); count != 2 {
		t.Fatalf("CountUnfinishedArticles after ReplaceFromRuns = %d, want 2", count)
	}

	// MarkArticleFailed triggers on-demand par2 release
	if err := j.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if len(j.DeferredRecoveryIndices()) != 0 {
		t.Fatalf("DeferredRecoveryIndices after MarkArticleFailed = %v, want empty", j.DeferredRecoveryIndices())
	}
	if j.Progress().Par2ReleaseReason() == "" {
		t.Fatal("Par2ReleaseReason should be set on failure release")
	}

	// DiscardDeferredPar2
	if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}
	if !j.DiscardDeferredPar2() {
		t.Fatal("DiscardDeferredPar2 should return true when changed")
	}
	if j.Progress().FileFetchPolicy(1) != FetchNever {
		t.Fatalf("Fetch policy = %v, want FetchNever", j.Progress().FileFetchPolicy(1))
	}

	// UndeferRecoveryVolumes
	if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}
	if err := j.UndeferRecoveryVolumes([]int{1}); err != nil {
		t.Fatalf("UndeferRecoveryVolumes: %v", err)
	}
	if j.Progress().FileFetchPolicy(1) != FetchAlways {
		t.Fatalf("Fetch policy = %v, want FetchAlways", j.Progress().FileFetchPolicy(1))
	}

	// IsComplete
	_ = j.MarkFileComplete(0)
	_ = j.MarkFileComplete(1)
	if !j.IsComplete() {
		t.Fatal("IsComplete should be true when all FetchAlways files are complete")
	}
}
