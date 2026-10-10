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
	if j.RepairState() != RepairUnknown {
		t.Errorf("RepairState() = %v, want RepairUnknown", j.RepairState())
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
	markWritten(t, j, 0)

	var visited []int32
	j.ForEachUnfinishedArticle(func(fileIdx int, artIdx int32, id string, bytes int, number int, subject string) bool {
		visited = append(visited, artIdx)
		return true
	})
	if len(visited) != 2 {
		t.Errorf("ForEachUnfinishedArticle visited %d articles, want 2", len(visited))
	}

	// RestoreFileMeta
	if err := j.RestoreFileMeta(0, "f1.rar", true, 0x1234); err != nil {
		t.Errorf("RestoreFileMeta: %v", err)
	}
	if err := j.RestoreFetchPolicy(0, FetchIfNeeded); err != nil {
		t.Errorf("RestoreFetchPolicy: %v", err)
	}
	if got := j.Progress().FileFetchPolicy(0); got != FetchIfNeeded {
		t.Errorf("FileFetchPolicy(0) after RestoreFetchPolicy = %v, want FetchIfNeeded", got)
	}
	if err := j.RestoreFetchPolicy(99, FetchIfNeeded); err == nil {
		t.Error("RestoreFetchPolicy with an out-of-range file index should error")
	}

	// RestoreContent needs a progress record of the job's own to restore onto.
	j2 := New("j3", "test3", Policy{})
	if err := j2.RestoreContent(m); err == nil {
		t.Error("RestoreContent on a job with no progress record: nil error")
	}
	if err := j.RestoreContent(nil); err == nil {
		t.Error("RestoreContent(nil): nil error")
	}
	other := newManifest([]JobFile{{Subject: "x.rar", Bytes: 100, Articles: []JobArticle{{ID: "<x>", Bytes: 100}}}})
	if err := j.RestoreContent(other); err == nil {
		t.Error("RestoreContent with a manifest of a different shape: nil error")
	}
	if err := j.RestoreContent(m); err != nil {
		t.Errorf("RestoreContent: %v", err)
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
	if err := j.RestoreFetchPolicy(0, FetchAlways); err == nil {
		t.Error("RestoreFetchPolicy on unattached job should error")
	}
	if err := j.MarkArticleWritten(durability.WrittenRow{}); err == nil {
		t.Error("MarkArticleWritten on unattached job should error")
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

	start := time.Unix(1700000000, 0).UTC()
	finish := time.Unix(1700000100, 0).UTC()

	// Production order, which is what #504 was about: dispatch.restore applies
	// the stored state before the job has any JobProgress, and AttachContent
	// seeds the fresh record from it. Restoring onto an already-hydrated job
	// is not a shape any caller produces.
	restoreM := NewManifest([]JobFile{{
		Subject:  "f1",
		Bytes:    100,
		Articles: []JobArticle{{ID: "<r1@x>", Bytes: 100, Number: 1}},
	}})
	jRestored := New("restored", "restored", Policy{})
	jRestored.RestoreProgressState("repair needed", start, finish, false)

	// Readable before hydration: the Job-level fields are authoritative while
	// progress is nil, which is what stops persistIfChanged zeroing the row
	// for a job that is never hydrated.
	if got := jRestored.DownloadStarted(); !got.Equal(start) {
		t.Errorf("DownloadStarted before hydration = %v, want %v", got, start)
	}
	if got := jRestored.DownloadFinished(); !got.Equal(finish) {
		t.Errorf("DownloadFinished before hydration = %v, want %v", got, finish)
	}
	if got := jRestored.Par2ReleaseReason(); got != "repair needed" {
		t.Errorf("Par2ReleaseReason before hydration = %q, want %q", got, "repair needed")
	}

	if err := jRestored.AttachContent(restoreM); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if got := jRestored.Progress().DownloadStarted(); !got.Equal(start) {
		t.Errorf("DownloadStarted after hydration = %v, want %v", got, start)
	}
	if got := jRestored.Progress().DownloadFinished(); !got.Equal(finish) {
		t.Errorf("DownloadFinished after hydration = %v, want %v", got, finish)
	}
	if got := jRestored.Progress().Par2ReleaseReason(); got != "repair needed" {
		t.Errorf("Par2ReleaseReason after hydration = %q, want %q", got, "repair needed")
	}

	// SetPar2ReleaseReason on a job with no progress must be readable back,
	// not silently dropped — the else branch in that setter.
	jEmpty := New("empty", "empty", Policy{})
	jEmpty.SetPar2ReleaseReason("set before hydration")
	if got := jEmpty.Par2ReleaseReason(); got != "set before hydration" {
		t.Errorf("Par2ReleaseReason = %q, want %q", got, "set before hydration")
	}
}

// TestResetForRetry_ClearsDownloadStamps pins that ResetForRetry clears both
// downloadStarted and downloadFinished stamps so that a retry can re-stamp them.
func TestResetForRetry_ClearsDownloadStamps(t *testing.T) {
	t.Parallel()

	m := NewManifest([]JobFile{
		{
			Subject: "test.rar",
			Bytes:   100,
			Articles: []JobArticle{
				{ID: "<a1@x>", Bytes: 100, Number: 1},
			},
		},
	})
	j := New("retry-job", "test.nzb", Policy{})
	start := time.Unix(1700000000, 0).UTC()
	finish := time.Unix(1700000100, 0).UTC()
	// Restore before attaching, so the stamps reach the JobProgress the way a
	// restarted job's do; then check ResetForRetry clears them.
	j.RestoreProgressState("", start, finish, false)
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if got := j.Progress().DownloadStarted(); !got.Equal(start) {
		t.Fatalf("DownloadStarted = %v, want %v", got, start)
	}
	if got := j.Progress().DownloadFinished(); !got.Equal(finish) {
		t.Fatalf("DownloadFinished = %v, want %v", got, finish)
	}

	j.ResetForRetry()

	if got := j.Progress().DownloadStarted(); !got.IsZero() {
		t.Errorf("DownloadStarted = %v after ResetForRetry, want zero", got)
	}
	if got := j.Progress().DownloadFinished(); !got.IsZero() {
		t.Errorf("DownloadFinished = %v after ResetForRetry, want zero", got)
	}
}

// TestResetForRetry_UncompletesAFileWithUndoneArticles: a file restored as
// Complete while one of its articles is not done loses Complete and its
// assembled CRC, so the article is dispatched again; a Complete file whose
// articles are all done keeps both.
func TestResetForRetry_UncompletesAFileWithUndoneArticles(t *testing.T) {
	t.Parallel()

	m := NewManifest([]JobFile{
		{
			Subject: "short.rar",
			Bytes:   200,
			Articles: []JobArticle{
				{ID: "<s1@x>", Bytes: 100, Number: 1},
				{ID: "<s2@x>", Bytes: 100, Number: 2},
			},
		},
		{
			Subject: "whole.rar",
			Bytes:   100,
			Articles: []JobArticle{
				{ID: "<w1@x>", Bytes: 100, Number: 1},
			},
		},
	})
	j := New("retry-short-file", "test.nzb", Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	// Article 1 and file 1's only article are done; article 0 is not.
	markWritten(t, j, 1)
	markWritten(t, j, 2)
	for fi, crc := range []uint32{0xAAAA, 0xBBBB} {
		if err := j.RestoreFileMeta(fi, "", true, crc); err != nil {
			t.Fatalf("RestoreFileMeta(%d): %v", fi, err)
		}
	}

	j.ResetForRetry()

	p := j.Progress()
	if p.FileComplete(0) {
		t.Error("file 0 is still Complete after ResetForRetry although article 0 is not done")
	}
	if got := p.FileAssembledCRC32(0); got != 0 {
		t.Errorf("file 0 AssembledCRC32 = %#x after ResetForRetry, want 0", got)
	}
	if !p.FileComplete(1) {
		t.Error("file 1 lost Complete although every article of it is done")
	}
	if got := p.FileAssembledCRC32(1); got != 0xBBBB {
		t.Errorf("file 1 AssembledCRC32 = %#x after ResetForRetry, want 0xbbbb", got)
	}
	if j.IsComplete() {
		t.Error("IsComplete() = true with article 0 undone")
	}
	var dispatched []int32
	if err := j.ForEachUnfinishedArticle(func(_ int, artIdx int32, _ string, _, _ int, _ string) bool {
		dispatched = append(dispatched, artIdx)
		return true
	}); err != nil {
		t.Fatalf("ForEachUnfinishedArticle: %v", err)
	}
	if len(dispatched) != 1 || dispatched[0] != 0 {
		t.Errorf("unfinished articles = %v, want [0]", dispatched)
	}
}

// TestResetForRetry_LeavesFetchNeverAlone pins that ResetForRetry no longer
// carries a fetch-policy branch: a retained FetchNever (e.g. from a clean
// par2 verdict on a retry path that inherited it) must not become
// FetchIfNeeded here. Ownership of a fresh job's policy belongs to
// BuildIngestJob's derivation, not to a repair branch in ResetForRetry — see
// the plan for #329. After Task 1 nothing on the production retry path can
// reach this branch with FetchNever any more (RestoreFileMeta no longer
// restores the policy), so this must be an internal/job unit test that calls
// ResetForRetry directly.
func TestResetForRetry_LeavesFetchNeverAlone(t *testing.T) {
	t.Parallel()

	m := NewManifest([]JobFile{
		{
			Subject: "recovery.vol000+01.par2",
			Bytes:   100,
			Articles: []JobArticle{
				{ID: "<a1@x>", Bytes: 100, Number: 1},
			},
		},
	})
	j := New("retry-fetch-never", "test.nzb", Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.SetFileFetchPolicy(0, FetchNever); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}

	j.ResetForRetry()

	if got := j.Progress().FileFetchPolicy(0); got != FetchNever {
		t.Errorf("FileFetchPolicy(0) = %v after ResetForRetry, want FetchNever — "+
			"ResetForRetry must not carry a fetch-policy branch", got)
	}
}

// TestMarkDownloadFinished_FirstWins pins the first-wins guard on
// MarkDownloadFinished / setDownloadFinishedOnce.
func TestMarkDownloadFinished_FirstWins(t *testing.T) {
	t.Parallel()

	m := NewManifest([]JobFile{
		{
			Subject: "test.rar",
			Bytes:   100,
			Articles: []JobArticle{
				{ID: "<a1@x>", Bytes: 100, Number: 1},
			},
		},
	})
	j := New("finish-job", "test.nzb", Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	first := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	second := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

	if err := j.MarkDownloadFinished(first); err != nil {
		t.Fatalf("MarkDownloadFinished(first): %v", err)
	}
	if got := j.Progress().DownloadFinished(); !got.Equal(first) {
		t.Fatalf("DownloadFinished = %v, want %v", got, first)
	}

	if err := j.MarkDownloadFinished(second); err != nil {
		t.Fatalf("MarkDownloadFinished(second): %v", err)
	}
	if got := j.Progress().DownloadFinished(); !got.Equal(first) {
		t.Errorf("DownloadFinished moved to %v on second call, want %v (first wins)", got, first)
	}
}

func TestJob_ProgressAccessors(t *testing.T) {
	t.Parallel()

	// 1. Unattached job returns zero values for all accessors
	jUnattached := New("unattached-job", "test.nzb", Policy{})
	if jUnattached.HasProgress() {
		t.Error("HasProgress() = true on unattached job, want false")
	}
	if got := jUnattached.ExpectedBytes(); got != 0 {
		t.Errorf("ExpectedBytes() = %d, want 0", got)
	}
	if got := jUnattached.RemainingBytes(); got != 0 {
		t.Errorf("RemainingBytes() = %d, want 0", got)
	}
	if got := jUnattached.FailedBytes(); got != 0 {
		t.Errorf("FailedBytes() = %d, want 0", got)
	}
	if got := jUnattached.ContentFailedBytes(); got != 0 {
		t.Errorf("ContentFailedBytes() = %d, want 0", got)
	}
	if got := jUnattached.PendingArticles(); got != 0 {
		t.Errorf("PendingArticles() = %d, want 0", got)
	}
	if got := jUnattached.DownloadStarted(); !got.IsZero() {
		t.Errorf("DownloadStarted() = %v, want zero", got)
	}
	if got := jUnattached.DownloadFinished(); !got.IsZero() {
		t.Errorf("DownloadFinished() = %v, want zero", got)
	}
	if got := jUnattached.Par2ReleaseReason(); got != "" {
		t.Errorf("Par2ReleaseReason() = %q, want empty", got)
	}
	if exp, rem, fail := jUnattached.ProgressFigures(); exp != 0 || rem != 0 || fail != 0 {
		t.Errorf("ProgressFigures() = (%d, %d, %d), want (0, 0, 0)", exp, rem, fail)
	}
	var nilProg *JobProgress
	if exp, rem, fail := nilProg.ProgressFigures(); exp != 0 || rem != 0 || fail != 0 {
		t.Errorf("nil JobProgress.ProgressFigures() = (%d, %d, %d), want (0, 0, 0)", exp, rem, fail)
	}

	// 2. Attached job returns proper values matching progress
	m := NewManifest([]JobFile{
		{
			Subject: "data.rar",
			Bytes:   100,
			Articles: []JobArticle{
				{ID: "<d1@x>", Bytes: 50, Number: 1},
				{ID: "<d2@x>", Bytes: 50, Number: 2},
			},
		},
		{
			Subject:        "data.vol01+01.par2",
			Bytes:          60,
			IsPar2Recovery: true,
			Deferred:       true,
			Articles: []JobArticle{
				{ID: "<p1@x>", Bytes: 60, Number: 3},
			},
		},
	})
	// PolicyFromPP(3): MarkArticleFailed below exercises the on-demand par2
	// release, which is gated on Policy.Repair; the zero Policy{} would
	// leave it unable to release, and this test wants the release path.
	j := New("attached-job", "test.nzb", PolicyFromPP(3))
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}

	if !j.HasProgress() {
		t.Fatal("HasProgress() = false on attached job, want true")
	}
	// Initial state: data.rar is FetchAlways (100 bytes, 2 articles), par2 is FetchIfNeeded (deferred, 0 bytes expected).
	if got := j.ExpectedBytes(); got != 100 {
		t.Errorf("ExpectedBytes() = %d, want 100", got)
	}
	if got := j.RemainingBytes(); got != 100 {
		t.Errorf("RemainingBytes() = %d, want 100", got)
	}
	if got := j.FailedBytes(); got != 0 {
		t.Errorf("FailedBytes() = %d, want 0", got)
	}
	if exp, rem, fail := j.ProgressFigures(); exp != 100 || rem != 100 || fail != 0 {
		t.Errorf("ProgressFigures() = (%d, %d, %d), want (100, 100, 0)", exp, rem, fail)
	}
	if got := j.ContentFailedBytes(); got != 0 {
		t.Errorf("ContentFailedBytes() = %d, want 0", got)
	}
	if got := j.PendingArticles(); got != 3 {
		t.Errorf("PendingArticles() = %d, want 3", got)
	}
	if got := j.DownloadStarted(); !got.IsZero() {
		t.Errorf("DownloadStarted() = %v, want zero", got)
	}
	if got := j.DownloadFinished(); !got.IsZero() {
		t.Errorf("DownloadFinished() = %v, want zero", got)
	}
	if got := j.Par2ReleaseReason(); got != "" {
		t.Errorf("Par2ReleaseReason() = %q, want empty", got)
	}

	// Mutate progress via Job methods
	start := time.Unix(1700000000, 0).UTC()
	finish := time.Unix(1700000100, 0).UTC()
	if err := j.MarkJobStarted(start); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	markWritten(t, j, 0)
	// MarkArticleFailed triggers on-demand par2 release and sets Par2ReleaseReason
	if err := j.MarkArticleFailed(1); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if err := j.MarkDownloadFinished(finish); err != nil {
		t.Fatalf("MarkDownloadFinished: %v", err)
	}

	if got := j.DownloadStarted(); !got.Equal(start) {
		t.Errorf("DownloadStarted() = %v, want %v", got, start)
	}
	if got := j.DownloadFinished(); !got.Equal(finish) {
		t.Errorf("DownloadFinished() = %v, want %v", got, finish)
	}
	if got := j.FailedBytes(); got != 50 {
		t.Errorf("FailedBytes() = %d, want 50", got)
	}
	if got := j.ContentFailedBytes(); got != 50 {
		t.Errorf("ContentFailedBytes() = %d, want 50", got)
	}
	if exp, rem, fail := j.ProgressFigures(); exp != j.ExpectedBytes() || rem != j.RemainingBytes() || fail != j.FailedBytes() {
		t.Errorf("ProgressFigures() = (%d, %d, %d), want (%d, %d, %d)", exp, rem, fail, j.ExpectedBytes(), j.RemainingBytes(), j.FailedBytes())
	}
	if got := j.Par2ReleaseReason(); got == "" {
		t.Error("Par2ReleaseReason() is empty after par2 release")
	}

	j.SetRecoveryBytes(1234)
	if got := j.RecoveryBytes(); got != 1234 {
		t.Errorf("RecoveryBytes() = %d, want 1234", got)
	}
}

// TestJobFileFetchPolicy_ReadsWithoutCloningProgress covers Job.FileFetchPolicy,
// which exists so a caller wanting one file's policy does not clone the whole
// JobProgress to get it.
//
// The two fallback branches are the point: a non-resident job and an
// out-of-range index both answer FetchAlways rather than erroring, because the
// zero policy means "fetch it". A caller that cannot learn the real answer
// downloads the file, which wastes bytes; the opposite default would silently
// skip a file the job needs.
func TestJobFileFetchPolicy_ReadsWithoutCloningProgress(t *testing.T) {
	unattached := New("unattached", "test", Policy{})
	if got := unattached.FileFetchPolicy(0); got != FetchAlways {
		t.Errorf("FileFetchPolicy(0) on a non-resident job = %v, want FetchAlways", got)
	}

	j := New("j1", "test", Policy{})
	m := newManifest([]JobFile{
		{Subject: "payload.bin", Bytes: 100, Articles: []JobArticle{{ID: "<a1@x>", Bytes: 100}}},
		{Subject: "payload.vol000+01.par2", Bytes: 100, Articles: []JobArticle{{ID: "<r1@x>", Bytes: 100}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if got := j.FileFetchPolicy(0); got != FetchAlways {
		t.Errorf("FileFetchPolicy(0) on a fresh job = %v, want the FetchAlways zero", got)
	}
	if err := j.SetFileFetchPolicy(1, FetchNever); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}
	if got := j.FileFetchPolicy(1); got != FetchNever {
		t.Errorf("FileFetchPolicy(1) = %v after setting FetchNever, want FetchNever", got)
	}
	// Must agree with the accessor it replaced at the seed sites, or seeding
	// would persist a different policy than SaveBatch later writes.
	if got, want := j.FileFetchPolicy(1), j.Progress().FileFetchPolicy(1); got != want {
		t.Errorf("Job.FileFetchPolicy(1) = %v but Progress().FileFetchPolicy(1) = %v; the two must not disagree", got, want)
	}
	for _, fi := range []int{-1, 2, 99} {
		if got := j.FileFetchPolicy(fi); got != FetchAlways {
			t.Errorf("FileFetchPolicy(%d) out of range = %v, want FetchAlways", fi, got)
		}
	}
}

// evictedThreeArticleJob builds a job with one 300-byte file of three 100-byte
// articles and no par2: articles 0 and 1 done, article 2 dispatched (its
// Emitted bit set) when the manifest is evicted. It returns the manifest so a
// test can re-hydrate with RestoreContent, as appResidency.Hydrate does with
// the one it re-reads from disk.
func evictedThreeArticleJob(t *testing.T) (*Job, *Manifest) {
	t.Helper()
	m := NewManifest([]JobFile{{Subject: "data.bin", Bytes: 300, Articles: []JobArticle{
		{ID: "<a0@x>", Bytes: 100, Number: 1},
		{ID: "<a1@x>", Bytes: 100, Number: 2},
		{ID: "<a2@x>", Bytes: 100, Number: 3},
	}}})
	j := New("evicted", "evicted", PolicyFromPP(3))
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	for _, i := range []int{0, 1} {
		markWritten(t, j, i)
	}
	if err := j.MarkArticleEmitted(2); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	j.Evict()
	if j.Resident() {
		t.Fatal("fixture: the job is still resident after Evict")
	}
	return j, m
}

// unfinishedArticles lists what ForEachUnfinishedArticle would offer the
// downloader.
func unfinishedArticles(t *testing.T, j *Job) []int32 {
	t.Helper()
	var out []int32
	if err := j.ForEachUnfinishedArticle(func(_ int, a int32, _ string, _ int, _ int, _ string) bool {
		out = append(out, a)
		return true
	}); err != nil {
		t.Fatalf("ForEachUnfinishedArticle: %v", err)
	}
	return out
}

// TestMarkArticleFailed_RecordsAFailureThatArrivesAfterEviction pins that a
// permanent failure reaching a job whose manifest was evicted mid-fetch is
// recorded, and that re-hydration charges it.
//
// The dispatcher can evict a job while its fetches are still in flight, so
// the failure can arrive after the manifest is gone.
// Hydration re-derives successes from the written_articles rows, but a
// failure lives only in its bit, so a refused failure left the article neither
// done nor failed, and the file could complete with failed=0 and RepairState
// intact: on a post with no par2, a hole nothing reports.
func TestMarkArticleFailed_RecordsAFailureThatArrivesAfterEviction(t *testing.T) {
	j, m := evictedThreeArticleJob(t)

	if err := j.MarkArticleFailed(2); err != nil {
		t.Fatalf("MarkArticleFailed on an evicted job: %v", err)
	}
	p := j.Progress()
	if !p.ArticleFailed(2) || !p.ArticleDone(2) {
		t.Fatalf("article 2 after an evicted failure: done=%v failed=%v, want both — "+
			"the failure was dropped, and nothing re-derives it at hydration",
			p.ArticleDone(2), p.ArticleFailed(2))
	}
	if p.ArticleEmitted(2) {
		t.Error("article 2 is still Emitted after its failure was recorded")
	}

	if err := j.RestoreContent(m); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}
	if got := j.FailedBytes(); got != 100 {
		t.Errorf("FailedBytes after re-hydration = %d, want 100", got)
	}
	if got := j.ContentFailedBytes(); got != 100 {
		t.Errorf("ContentFailedBytes after re-hydration = %d, want 100", got)
	}
	if got := j.RepairState(); got == RepairIntact {
		t.Errorf("RepairState after re-hydration = %s: a failed article on a post with no par2 reads as undamaged", got)
	}
	if got := j.PendingArticles(); got != 0 {
		t.Errorf("PendingArticles after re-hydration = %d, want 0", got)
	}
	if got := unfinishedArticles(t, j); len(got) != 0 {
		t.Errorf("unfinished articles after re-hydration = %v, want none: a failed article is resolved", got)
	}
}

// TestMarkArticleFailed_EvictedRejectsAnOutOfRangeIndex pins that the evicted
// path still bounds the index, against the progress record's own size.
func TestMarkArticleFailed_EvictedRejectsAnOutOfRangeIndex(t *testing.T) {
	j, _ := evictedThreeArticleJob(t)
	for _, idx := range []int{-1, 3} {
		if err := j.MarkArticleFailed(idx); err == nil {
			t.Errorf("MarkArticleFailed(%d) on an evicted three-article job: nil error", idx)
		}
		if err := j.ClearArticleEmitted(idx); err == nil {
			t.Errorf("ClearArticleEmitted(%d) on an evicted three-article job: nil error", idx)
		}
	}
	p := j.Progress()
	for i := range 3 {
		if p.ArticleFailed(i) {
			t.Errorf("an out-of-range failure set article %d's failed bit", i)
		}
	}
}

// TestClearArticleEmitted_ReturnsAnEvictedArticleToOutstanding pins the
// sibling: a result that will not be written for a job whose manifest was
// evicted returns its article to Outstanding.
//
// Eviction keeps the Emitted bit, and ForEachUnfinishedArticle skips an
// article whose bit is set, so a refused clear left the article undispatchable
// after re-hydration until a downloader reload or a restart.
func TestClearArticleEmitted_ReturnsAnEvictedArticleToOutstanding(t *testing.T) {
	j, m := evictedThreeArticleJob(t)

	if err := j.ClearArticleEmitted(2); err != nil {
		t.Fatalf("ClearArticleEmitted on an evicted job: %v", err)
	}
	if j.Progress().ArticleEmitted(2) {
		t.Fatal("article 2 is still Emitted after the clear")
	}

	if err := j.RestoreContent(m); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}
	if got := unfinishedArticles(t, j); len(got) != 1 || got[0] != 2 {
		t.Errorf("unfinished articles after re-hydration = %v, want [2]", got)
	}
	if got := j.PendingArticles(); got != 1 {
		t.Errorf("PendingArticles after re-hydration = %d, want 1", got)
	}
	if p := j.Progress(); p.ArticleDone(2) || p.ArticleFailed(2) {
		t.Errorf("article 2 done=%v failed=%v after a clear; a clear resolves nothing", p.ArticleDone(2), p.ArticleFailed(2))
	}
}

// TestRestoreContent_KeepsWritesMadeWhileEvicted pins that re-hydration
// restores onto the job's own progress record rather than installing a copy.
//
// Hydration used to clone the record, then install the clone. Every write
// that needs no manifest and landed between the two went to the record the
// install replaced: a failed bit lost, an article left Emitted, a stamp or a
// par2 reason gone. This drives each such write in that position and requires
// all of them after RestoreContent.
func TestRestoreContent_KeepsWritesMadeWhileEvicted(t *testing.T) {
	j, m := evictedThreeArticleJob(t)
	if err := j.MarkArticleEmitted(1); err == nil {
		t.Fatal("fixture: MarkArticleEmitted succeeded on an evicted job")
	}

	started := time.Unix(1_700_000_000, 0)
	if err := j.MarkArticleFailed(2); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if err := j.MarkJobStarted(started); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if err := j.RecordDownload("srv", 42); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}
	j.SetPar2ReleaseReason("evicted reason")
	if err := j.SetFileFetchPolicy(0, FetchNever); err != nil {
		t.Fatalf("SetFileFetchPolicy: %v", err)
	}

	if err := j.RestoreContent(m); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}

	p := j.Progress()
	if !p.ArticleFailed(2) || p.ArticleEmitted(2) {
		t.Errorf("article 2 after re-hydration: failed=%v emitted=%v, want failed and not emitted — "+
			"the failure recorded while evicted was replaced by a stale record", p.ArticleFailed(2), p.ArticleEmitted(2))
	}
	if got := j.FailedBytes(); got != 100 {
		t.Errorf("FailedBytes after re-hydration = %d, want 100", got)
	}
	if got := j.DownloadStarted(); !got.Equal(started) {
		t.Errorf("DownloadStarted after re-hydration = %v, want %v", got, started)
	}
	if got := p.ServerStats()["srv"]; got != 42 {
		t.Errorf("server bytes after re-hydration = %d, want 42", got)
	}
	if got := j.Par2ReleaseReason(); got != "evicted reason" {
		t.Errorf("Par2ReleaseReason after re-hydration = %q, want %q", got, "evicted reason")
	}
	if got := j.FileFetchPolicy(0); got != FetchNever {
		t.Errorf("FileFetchPolicy(0) after re-hydration = %v, want FetchNever", got)
	}
}

// TestMarkArticleFailed_ResidentEmittedArticleLeavesPendingOnce pins
// markFailed's emitted accounting: an article left the pending count when it
// was emitted, so failing it must not take it out again. A double decrement
// drives the counters below the real outstanding work, and
// ForEachUnfinishedArticle returns at once when pendingArticles reaches zero,
// stranding the articles still to fetch.
func TestMarkArticleFailed_ResidentEmittedArticleLeavesPendingOnce(t *testing.T) {
	m := NewManifest([]JobFile{{Subject: "data.bin", Bytes: 200, Articles: []JobArticle{
		{ID: "<b0@x>", Bytes: 100, Number: 1},
		{ID: "<b1@x>", Bytes: 100, Number: 2},
	}}})
	j := New("resident", "resident", PolicyFromPP(3))
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.MarkArticleEmitted(1); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	if got := j.PendingArticles(); got != 1 {
		t.Fatalf("fixture: PendingArticles after emitting one of two = %d, want 1", got)
	}

	if err := j.MarkArticleFailed(1); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}

	if got := j.PendingArticles(); got != 1 {
		t.Errorf("PendingArticles after failing the emitted article = %d, want 1: "+
			"the article was taken out of the count twice", got)
	}
	if got := j.Progress().FilePending(0); got != 1 {
		t.Errorf("FilePending(0) = %d, want 1", got)
	}
	if got := unfinishedArticles(t, j); len(got) != 1 || got[0] != 0 {
		t.Errorf("unfinished articles = %v, want [0]", got)
	}
}

// TestClearDownloadFinished_ReopensTheFinishAndKeepsTheStart: a job going back
// to Fetching gives up its finish, keeps the start its first article set, and
// takes a new finish when it leaves again.
func TestClearDownloadFinished_ReopensTheFinishAndKeepsTheStart(t *testing.T) {
	t.Parallel()

	m := NewManifest([]JobFile{{
		Subject:  "test.rar",
		Bytes:    100,
		Articles: []JobArticle{{ID: "<a1@x>", Bytes: 100, Number: 1}},
	}})
	j := New("clear-job", "test.nzb", Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	first := start.Add(time.Hour)
	second := start.Add(2 * time.Hour)
	if err := j.MarkJobStarted(start); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if err := j.MarkDownloadFinished(first); err != nil {
		t.Fatalf("MarkDownloadFinished(first): %v", err)
	}

	if err := j.ClearDownloadFinished(); err != nil {
		t.Fatalf("ClearDownloadFinished: %v", err)
	}
	if got := j.DownloadFinished(); !got.IsZero() {
		t.Errorf("DownloadFinished after the clear = %v, want zero", got)
	}
	if got := j.DownloadStarted(); !got.Equal(start) {
		t.Errorf("DownloadStarted after the clear = %v, want %v", got, start)
	}
	if err := j.MarkDownloadFinished(second); err != nil {
		t.Fatalf("MarkDownloadFinished(second): %v", err)
	}
	if got := j.DownloadFinished(); !got.Equal(second) {
		t.Errorf("DownloadFinished after the re-stamp = %v, want %v", got, second)
	}
}
