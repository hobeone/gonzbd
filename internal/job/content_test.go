package job

import (
	"testing"
	"time"

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

func TestJob_AdditionalMethods(t *testing.T) {
	now := time.Now()
	j := New("j2", "orig-name", PolicyFromPP(1))
	j.SetName("new-name")
	if j.Name() != "new-name" {
		t.Errorf("Name() = %q, want new-name", j.Name())
	}
	j.SetPolicy(PolicyFromPP(2))
	if !j.Policy().Unpack {
		t.Error("Policy().Unpack should be true for PP 2")
	}
	j.SetAdded(now)
	if !j.Added().Equal(now) {
		t.Errorf("Added() = %v, want %v", j.Added(), now)
	}

	// Unattached calls
	if j.TotalBytes() != 0 || j.RecoveryBytes() != 0 || j.RecoveryFiles() != 0 {
		t.Error("expected 0 for unattached byte figures")
	}
	if j.RepairState() != RepairIntact {
		t.Errorf("RepairState() = %v, want RepairIntact", j.RepairState())
	}
	if j.HasDeferredPar2() || j.UsesOnDemandPar2() {
		t.Error("expected false for unattached par2 checks")
	}
	j.SetPar2ReleaseReason("test")
	j.ClearEmittedForReload(false)
	if j.CheckEarlyAbort() {
		t.Error("CheckEarlyAbort should be false for unattached job")
	}
	j.ResetForRetry()

	// Timing methods
	j.MarkJobStarted(now)
	_ = j.RecordDownload("srv1", 500)
	j.MarkDownloadFinished(now.Add(time.Second))

	// Attach content
	m := NewManifest([]JobFile{
		{
			Subject: "f1.rar",
			Bytes:   100,
			Articles: []JobArticle{
				{ID: "<art1@x>", Bytes: 50, Number: 1},
				{ID: "<art2@x>", Bytes: 50, Number: 2},
			},
		},
		{
			Subject:        "f2.vol01+01.par2",
			Bytes:          100,
			IsPar2Recovery: true,
			Deferred:       true,
			Articles: []JobArticle{
				{ID: "<art3@x>", Bytes: 100, Number: 3},
			},
		},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if j.TotalBytes() != 200 {
		t.Errorf("TotalBytes() = %d, want 200", j.TotalBytes())
	}
	if j.RecoveryBytes() != 100 {
		t.Errorf("RecoveryBytes() = %d, want 100", j.RecoveryBytes())
	}
	if j.RecoveryFiles() != 1 {
		t.Errorf("RecoveryFiles() = %d, want 1", j.RecoveryFiles())
	}
	if j.UsesOnDemandPar2() {
		t.Error("UsesOnDemandPar2() should be false initially")
	}
	if j.Progress().NumFiles() != 2 {
		t.Errorf("NumFiles() = %d, want 2", j.Progress().NumFiles())
	}
	if j.Progress().UsesOnDemandPar2() {
		t.Error("j.Progress().UsesOnDemandPar2() should be false initially")
	}

	// Article operations
	if err := j.MarkArticleEmitted(0); err != nil {
		t.Errorf("MarkArticleEmitted: %v", err)
	}
	if err := j.ClearArticleEmitted(0); err != nil {
		t.Errorf("ClearArticleEmitted: %v", err)
	}
	if err := j.MarkArticleDone(0, 50, "srv1"); err != nil {
		t.Errorf("MarkArticleDone: %v", err)
	}

	var visited []int32
	j.ForEachUnfinishedArticle(func(fileIdx int, artIdx int32, id string, bytes int, number int, subject string) bool {
		visited = append(visited, artIdx)
		return true
	})
	if len(visited) != 2 {
		t.Errorf("ForEachUnfinishedArticle visited %d articles, want 2", len(visited))
	}

	// RestoreFileMeta & ApplyResolution
	if err := j.RestoreFileMeta(0, "f1.rar", true, 0x1234, FetchAlways); err != nil {
		t.Errorf("RestoreFileMeta: %v", err)
	}
	if err := j.ApplyResolution([]RunRange{{First: 0, Last: 0}}, []int32{1}); err != nil {
		t.Errorf("ApplyResolution: %v", err)
	}

	// RestoreContent
	p := j.Progress()
	j2 := New("j3", "test3", Policy{})
	if err := j2.RestoreContent(m, p); err != nil {
		t.Errorf("RestoreContent: %v", err)
	}

	// Checkpoint
	cp := j.Checkpoint()
	if cp.ID != "j2" {
		t.Errorf("Checkpoint ID = %q, want j2", cp.ID)
	}

	// ResetForRetry when resident
	j.ResetForRetry()

	// Par2 helpers
	if !IsPar2File("test.par2") || !IsPar2File("test.vol01+02.par2") {
		t.Error("IsPar2File should return true for .par2 files")
	}
	if IsPar2File("test.rar") {
		t.Error("IsPar2File should return false for .rar files")
	}

	files := []JobFile{
		{Subject: "b.rar"},
		{Subject: "a.rar"},
	}
	SortJobFiles(files)
	if files[0].Subject != "a.rar" {
		t.Errorf("SortJobFiles order: %s, want a.rar first", files[0].Subject)
	}
}

func TestContentMethods_UnattachedJobAndRunsErrors(t *testing.T) {
	j := New("unattached", "test", Policy{})

	if err := j.SetFileFetchPolicy(0, FetchAlways); err == nil {
		t.Error("SetFileFetchPolicy on unattached job should error")
	}
	if err := j.MarkArticleDone(0, 100, "srv"); err == nil {
		t.Error("MarkArticleDone on unattached job should error")
	}
	if err := j.MarkArticleEmitted(0); err == nil {
		t.Error("MarkArticleEmitted on unattached job should error")
	}
	if err := j.ClearArticleEmitted(0); err == nil {
		t.Error("ClearArticleEmitted on unattached job should error")
	}
	if err := j.ForEachUnfinishedArticle(func(int, int32, string, int, int, string) bool { return true }); err == nil {
		t.Error("ForEachUnfinishedArticle on unattached job should error")
	}
	if err := j.markFileComplete(0); err == nil {
		t.Error("markFileComplete on unattached job should error")
	}
	if err := j.UndeferRecoveryVolumes(nil); err == nil {
		t.Error("UndeferRecoveryVolumes on unattached job should error")
	}
	j.SetPar2ReleaseReason("reason")
	if err := j.SetFileFilename(0, "fn"); err == nil {
		t.Error("SetFileFilename on unattached job should error")
	}
	if _, err := j.SetFileCRC32FromRuns(0, nil); err == nil {
		t.Error("SetFileCRC32FromRuns on unattached job should error")
	}
	if err := j.ReplaceFromRuns(nil, nil); err == nil {
		t.Error("ReplaceFromRuns on unattached job should error")
	}
	if cleared, retained := j.ClearEmittedForReload(false); cleared != nil || retained != nil {
		t.Error("ClearEmittedForReload on unattached job should return nil, nil")
	}
	if j.IsComplete() {
		t.Error("IsComplete on unattached job should be false")
	}
	if err := j.MarkJobStarted(time.Now()); err == nil {
		t.Error("MarkJobStarted on unattached job should error")
	}
	if err := j.RecordDownload("srv", 100); err == nil {
		t.Error("RecordDownload on unattached job should error")
	}
	if err := j.MarkDownloadFinished(time.Now()); err == nil {
		t.Error("MarkDownloadFinished on unattached job should error")
	}

	m := newManifest([]JobFile{
		{Subject: "f.rar", Bytes: 200, Articles: []JobArticle{{ID: "<1>", Bytes: 100}, {ID: "<2>", Bytes: 100}}},
	})
	jResident := New("res", "test", Policy{})
	if err := jResident.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if err := jResident.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	// Mark file 0 complete so the failed article is retained rather than reset
	jResident.progress.files[0].Complete = true

	cleared, retained := jResident.ClearEmittedForReload(true)
	if len(retained) != 1 || retained[0] != 0 {
		t.Errorf("retained = %v, want [0]", retained)
	}
	_ = cleared

	badRunFi := durability.Run{FileIdx: 99, FirstArtIdx: 0, LastArtIdx: 1}
	if err := jResident.SeedFromRuns([]durability.Run{badRunFi}); err == nil {
		t.Error("SeedFromRuns should error on bad file index run")
	}
	badRunArt := durability.Run{FileIdx: 0, FirstArtIdx: 5, LastArtIdx: 2}
	if err := jResident.SeedFromRuns([]durability.Run{badRunArt}); err == nil {
		t.Error("SeedFromRuns should error on out-of-range article indices run")
	}

	jResident.SetPar2ReleaseReason("clean")
	if err := jResident.MarkJobStarted(time.Now()); err != nil {
		t.Errorf("MarkJobStarted: %v", err)
	}
	if err := jResident.RecordDownload("srv", 50); err != nil {
		t.Errorf("RecordDownload: %v", err)
	}
	if err := jResident.MarkDownloadFinished(time.Now()); err != nil {
		t.Errorf("MarkDownloadFinished: %v", err)
	}

	if _, err := jResident.SetFileCRC32FromRuns(99, nil); err == nil {
		t.Error("SetFileCRC32FromRuns should error on out-of-range fileIdx")
	}
	validCRC := durability.Run{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, CRC32: 0x12345678}
	if ok, err := jResident.SetFileCRC32FromRuns(0, []durability.Run{validCRC}); err != nil || !ok {
		t.Errorf("SetFileCRC32FromRuns(valid) = %v, %v; want true, nil", ok, err)
	}

	if err := jResident.ReplaceFromRuns([]int32{99}, nil); err == nil {
		t.Error("ReplaceFromRuns should error on out of range file index")
	}
	validRun := durability.Run{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0}
	if err := jResident.ReplaceFromRuns([]int32{0}, []durability.Run{validRun}); err != nil {
		t.Errorf("ReplaceFromRuns: %v", err)
	}
}
