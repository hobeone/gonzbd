package queue

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// makeMultiFileJob creates a job with nFiles files, each with nArticles
// articles. Article message-IDs follow the pattern "f<fileIdx>a<artIdx>@test".
// Each article is 100_000 bytes so math is predictable.
func makeMultiFileJob(t *testing.T, name string, nFiles, nArticles int) *Job {
	t.Helper()
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {name}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	for fi := range nFiles {
		f := nzb.File{
			Subject: name + " - file " + string(rune('A'+fi)),
			Date:    time.Unix(1700000000, 0),
		}
		for ai := range nArticles {
			art := nzb.Article{
				ID:     articleID(fi, ai),
				Bytes:  100_000,
				Number: ai + 1,
			}
			f.Articles = append(f.Articles, art)
			f.Bytes += int64(art.Bytes)
		}
		parsed.Files = append(parsed.Files, f)
	}
	job, err := NewJob(parsed, AddOptions{
		Filename: name + ".nzb",
		Priority: constants.NormalPriority,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	return job
}

func articleID(fileIdx, artIdx int) string {
	return "f" + string(rune('0'+fileIdx)) + "a" + string(rune('0'+artIdx)) + "@test"
}

// ---------- ExistsByName / ExistsByMD5 ----------

func TestExistsByName(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "target", constants.NormalPriority)
	_ = q.Add(j)

	tests := []struct {
		name string
		want bool
	}{
		{"target", true},
		{"Target", false}, // case-sensitive
		{"nonexistent", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := q.ExistsByName(tc.name); got != tc.want {
				t.Errorf("ExistsByName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestExistsByName_AfterRemove(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "removed", constants.NormalPriority)
	_ = q.Add(j)

	if !q.ExistsByName("removed") {
		t.Fatal("should exist before remove")
	}
	_ = q.Remove(j.ID)
	if q.ExistsByName("removed") {
		t.Error("should not exist after remove")
	}
}

func TestExistsByMD5(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "md5check", constants.NormalPriority)
	_ = q.Add(j)

	if !q.ExistsByMD5(j.MD5) {
		t.Error("ExistsByMD5 should find existing job's MD5")
	}
	if q.ExistsByMD5("0000000000000000000000000000dead") {
		t.Error("ExistsByMD5 should not find nonexistent MD5")
	}
	if q.ExistsByMD5("") {
		t.Error("ExistsByMD5 should not match empty string")
	}
}

// ---------- CountUnfinishedArticles ----------

func TestCountUnfinishedArticles(t *testing.T) {
	t.Parallel()
	q := New()
	// 2 files, 4 articles each
	j := makeMultiFileJob(t, "count", 2, 4)
	_ = q.Add(j)

	t.Run("all unfinished initially", func(t *testing.T) {
		count, err := q.CountUnfinishedArticles(j.ID, 0)
		if err != nil {
			t.Fatalf("CountUnfinishedArticles: %v", err)
		}
		if count != 4 {
			t.Errorf("count = %d, want 4", count)
		}
	})

	t.Run("count decreases after MarkArticleDone", func(t *testing.T) {
		ackDone(t, q, j.ID, articleID(0, 0))
		ackDone(t, q, j.ID, articleID(0, 1))
		count, err := q.CountUnfinishedArticles(j.ID, 0)
		if err != nil {
			t.Fatalf("CountUnfinishedArticles: %v", err)
		}
		if count != 2 {
			t.Errorf("count = %d, want 2 after marking 2 done", count)
		}
	})

	t.Run("count decreases after MarkArticleFailed", func(t *testing.T) {
		ackFailed(t, q, j.ID, articleID(0, 2))
		count, err := q.CountUnfinishedArticles(j.ID, 0)
		if err != nil {
			t.Fatalf("CountUnfinishedArticles: %v", err)
		}
		if count != 1 {
			t.Errorf("count = %d, want 1 (2 done + 1 failed = 3 finished)", count)
		}
	})

	t.Run("file 1 unaffected by file 0 changes", func(t *testing.T) {
		count, err := q.CountUnfinishedArticles(j.ID, 1)
		if err != nil {
			t.Fatalf("CountUnfinishedArticles file 1: %v", err)
		}
		if count != 4 {
			t.Errorf("file 1 count = %d, want 4 (untouched)", count)
		}
	})

	t.Run("zero after all done", func(t *testing.T) {
		ackDone(t, q, j.ID, articleID(0, 3))
		count, err := q.CountUnfinishedArticles(j.ID, 0)
		if err != nil {
			t.Fatalf("CountUnfinishedArticles: %v", err)
		}
		if count != 0 {
			t.Errorf("count = %d, want 0 (all finished)", count)
		}
	})

	t.Run("error on invalid job", func(t *testing.T) {
		_, err := q.CountUnfinishedArticles("nonexistent", 0)
		if err == nil {
			t.Error("expected error for nonexistent job")
		}
	})

	t.Run("error on invalid file index", func(t *testing.T) {
		_, err := q.CountUnfinishedArticles(j.ID, mustManifest(t, j).NumFiles())
		if err == nil {
			t.Error("expected error for out-of-range file index")
		}
	})

	t.Run("error on negative file index", func(t *testing.T) {
		_, err := q.CountUnfinishedArticles(j.ID, -1)
		if err == nil {
			t.Error("expected error for negative file index")
		}
	})
}

// ---------- MarkArticleEmitted / ClearArticleEmitted / ClearAllEmitted ----------

func TestMarkArticleEmitted(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "emit", 1, 3)
	_ = q.Add(j)

	msgID := articleID(0, 0)

	t.Run("emitted article is skipped by ForEachUnfinished", func(t *testing.T) {
		if err := q.MarkArticleEmitted(j.ID, msgID); err != nil {
			t.Fatalf("MarkArticleEmitted: %v", err)
		}
		var yielded []string
		q.ForEachUnfinishedArticle(func(ua UnfinishedArticle) bool {
			yielded = append(yielded, ua.MessageID)
			return true
		})
		for _, id := range yielded {
			if id == msgID {
				t.Errorf("emitted article %q should NOT be yielded", msgID)
			}
		}
		if len(yielded) != 2 {
			t.Errorf("expected 2 unemitted articles, got %d", len(yielded))
		}
	})

	t.Run("idempotent: re-emitting is no-op", func(t *testing.T) {
		if err := q.MarkArticleEmitted(j.ID, msgID); err != nil {
			t.Errorf("re-emit should not error: %v", err)
		}
	})

	t.Run("error on nonexistent job", func(t *testing.T) {
		if err := q.MarkArticleEmitted("bogus", msgID); err == nil {
			t.Error("expected error for nonexistent job")
		}
	})

	t.Run("error on nonexistent article", func(t *testing.T) {
		if err := q.MarkArticleEmitted(j.ID, "nosuch@test"); err == nil {
			t.Error("expected error for nonexistent article")
		}
	})
}

func TestClearArticleEmitted(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "clear-emit", 1, 2)
	_ = q.Add(j)
	// Drain the add notification.
	<-q.Notify()

	msg0 := articleID(0, 0)

	// Emit, then clear.
	_ = q.MarkArticleEmitted(j.ID, msg0)
	if err := q.ClearArticleEmitted(j.ID, msg0); err != nil {
		t.Fatalf("ClearArticleEmitted: %v", err)
	}

	// After clearing, the article should be yielded again.
	var yielded []string
	q.ForEachUnfinishedArticle(func(ua UnfinishedArticle) bool {
		yielded = append(yielded, ua.MessageID)
		return true
	})
	found := false
	for _, id := range yielded {
		if id == msg0 {
			found = true
		}
	}
	if !found {
		t.Errorf("cleared article %q should be yielded by ForEachUnfinished", msg0)
	}

	// ClearArticleEmitted must signal the dispatcher.
	select {
	case <-q.Notify():
	case <-time.After(time.Second):
		t.Error("ClearArticleEmitted should signal notify channel")
	}

	// Error cases.
	if err := q.ClearArticleEmitted("bogus", msg0); err == nil {
		t.Error("expected error for nonexistent job")
	}
	if err := q.ClearArticleEmitted(j.ID, "nosuch@test"); err == nil {
		t.Error("expected error for nonexistent article")
	}
}

func TestClearAllEmitted(t *testing.T) {
	t.Parallel()
	q := New()
	j1 := makeMultiFileJob(t, "clear-all-1", 1, 2)
	j2 := makeMultiFileJob(t, "clear-all-2", 1, 2)
	_ = q.Add(j1)
	_ = q.Add(j2)

	// Emit every article in both jobs.
	for i := range mustManifest(t, j1).NumArticles() {
		_ = q.MarkArticleEmitted(j1.ID, mustManifest(t, j1).ArticleID(i))
	}
	for i := range mustManifest(t, j2).NumArticles() {
		_ = q.MarkArticleEmitted(j2.ID, mustManifest(t, j2).ArticleID(i))
	}

	// ForEach should yield nothing: all emitted.
	countBefore := 0
	q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
		countBefore++
		return true
	})
	if countBefore != 0 {
		t.Errorf("expected 0 unfinished before ClearAllEmitted, got %d", countBefore)
	}

	// Drain any stale notification.
	select {
	case <-q.Notify():
	default:
	}

	q.ClearAllEmitted()

	// All articles should now be yielded.
	countAfter := 0
	q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
		countAfter++
		return true
	})
	if countAfter != 4 { // 2 jobs × 1 file × 2 articles
		t.Errorf("expected 4 unfinished after ClearAllEmitted, got %d", countAfter)
	}

	// ClearAllEmitted must signal.
	select {
	case <-q.Notify():
	case <-time.After(time.Second):
		t.Error("ClearAllEmitted should signal notify channel")
	}
}

// ---------- ForEachUnfinishedArticle ----------

func TestForEachUnfinishedArticle(t *testing.T) {
	t.Parallel()
	q := New()
	q.PauseAll()
	j := makeMultiFileJob(t, "foreach", 2, 3)
	_ = q.Add(j)

	t.Run("yields all articles initially", func(t *testing.T) {
		var count int
		q.ForEachUnfinishedArticle(func(ua UnfinishedArticle) bool {
			count++
			if ua.JobID != j.ID {
				t.Errorf("wrong job ID: %s", ua.JobID)
			}
			if ua.JobStatus != constants.StatusQueued {
				t.Errorf("wrong status: %s", ua.JobStatus)
			}
			return true
		})
		if count != 6 { // 2 files × 3 articles
			t.Errorf("count = %d, want 6", count)
		}
	})

	t.Run("skips Done articles", func(t *testing.T) {
		ackDone(t, q, j.ID, articleID(0, 0))
		ackDone(t, q, j.ID, articleID(1, 2))
		var count int
		q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
			count++
			return true
		})
		if count != 4 {
			t.Errorf("count = %d, want 4 (2 done)", count)
		}
	})

	t.Run("skips Emitted articles", func(t *testing.T) {
		_ = q.MarkArticleEmitted(j.ID, articleID(0, 1))
		var count int
		q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
			count++
			return true
		})
		if count != 3 {
			t.Errorf("count = %d, want 3 (2 done + 1 emitted)", count)
		}
	})

	t.Run("skips complete files", func(t *testing.T) {
		_ = q.MarkFileComplete(j.ID, 0)
		var count int
		seenFileIdx := -1
		q.ForEachUnfinishedArticle(func(ua UnfinishedArticle) bool {
			count++
			seenFileIdx = ua.FileIdx
			return true
		})
		if count != 2 {
			t.Errorf("count = %d, want 2 (file 0 complete, file 1 has 1 done)", count)
		}
		if seenFileIdx != 1 {
			t.Errorf("all remaining articles should be from file 1, got fileIdx=%d", seenFileIdx)
		}
	})

	t.Run("skips PostProc jobs", func(t *testing.T) {
		_ = q.SetStatus(j.ID, constants.StatusDownloading)
		ok, err := q.SetPostProcStarted(j.ID)
		if err != nil || !ok {
			t.Fatalf("SetPostProcStarted: %v, %v", ok, err)
		}
		var count int
		q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
			count++
			return true
		})
		if count != 0 {
			t.Errorf("count = %d, want 0 (job in post-proc)", count)
		}
	})

	t.Run("early stop on false return", func(t *testing.T) {
		// Add a fresh job with known articles.
		fresh := makeMultiFileJob(t, "early-stop", 1, 5)
		_ = q.Add(fresh)
		var count int
		q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
			count++
			return count < 3 // stop after 3
		})
		if count != 3 {
			t.Errorf("count = %d, want 3 (early stop)", count)
		}
	})
}

func TestForEachUnfinishedArticle_IncludesPausedJobs(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "paused-foreach", 1, 2)
	_ = q.Add(j)
	_ = q.Pause(j.ID)

	var count int
	var statuses []constants.Status
	q.ForEachUnfinishedArticle(func(ua UnfinishedArticle) bool {
		count++
		statuses = append(statuses, ua.JobStatus)
		return true
	})
	if count != 2 {
		t.Errorf("paused job should still yield articles, got %d", count)
	}
	for _, s := range statuses {
		if s != constants.StatusPaused {
			t.Errorf("status should be Paused, got %s", s)
		}
	}
}

// ---------- TotalRemainingBytes ----------

func TestTotalRemainingBytes(t *testing.T) {
	t.Parallel()
	q := New()

	t.Run("empty queue", func(t *testing.T) {
		if got := q.TotalRemainingBytes(); got != 0 {
			t.Errorf("empty queue: TotalRemainingBytes = %d, want 0", got)
		}
	})

	// Each article is 100_000 bytes.
	// Job a: 2 files × 3 articles = 600_000 bytes total.
	a := makeMultiFileJob(t, "a", 2, 3)
	_ = q.Add(a)

	t.Run("initial value matches total", func(t *testing.T) {
		if got := q.TotalRemainingBytes(); got != 600_000 {
			t.Errorf("TotalRemainingBytes = %d, want 600000", got)
		}
	})

	// Mark one article done → remaining should decrease by 100_000.
	ackDone(t, q, a.ID, articleID(0, 0))

	t.Run("decreases after MarkArticleDone", func(t *testing.T) {
		if got := q.TotalRemainingBytes(); got != 500_000 {
			t.Errorf("TotalRemainingBytes = %d, want 500000", got)
		}
	})

	// Mark one article failed → remaining also decreases.
	ackFailed(t, q, a.ID, articleID(0, 1))

	t.Run("decreases after MarkArticleFailed", func(t *testing.T) {
		if got := q.TotalRemainingBytes(); got != 400_000 {
			t.Errorf("TotalRemainingBytes = %d, want 400000", got)
		}
	})

	// Add another job.
	b := makeMultiFileJob(t, "b", 1, 2) // 200_000 bytes
	_ = q.Add(b)

	t.Run("sums across multiple jobs", func(t *testing.T) {
		if got := q.TotalRemainingBytes(); got != 600_000 { // 400k + 200k
			t.Errorf("TotalRemainingBytes = %d, want 600000", got)
		}
	})
}

// ---------- GetJobStatus ----------

func TestGetJobStatus(t *testing.T) {
	t.Parallel()
	q := New()
	q.PauseAll()
	j := makeJob(t, "status-check", constants.NormalPriority)
	_ = q.Add(j)

	status, err := q.GetJobStatus(j.ID)
	if err != nil {
		t.Fatalf("GetJobStatus: %v", err)
	}
	if status != constants.StatusQueued {
		t.Errorf("status = %q, want Queued", status)
	}

	_ = q.Pause(j.ID)
	status, err = q.GetJobStatus(j.ID)
	if err != nil {
		t.Fatalf("GetJobStatus after pause: %v", err)
	}
	if status != constants.StatusPaused {
		t.Errorf("status = %q, want Paused", status)
	}

	_, err = q.GetJobStatus("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent job")
	}
}

// ---------- SetPostProcStarted ----------

func TestSetPostProcStarted(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "postproc", constants.NormalPriority)
	_ = q.Add(j)
	_ = q.SetStatus(j.ID, constants.StatusDownloading)

	t.Run("first call returns true", func(t *testing.T) {
		ok, err := q.SetPostProcStarted(j.ID)
		if err != nil {
			t.Fatalf("SetPostProcStarted: %v", err)
		}
		if !ok {
			t.Error("expected true on first call")
		}
	})

	t.Run("second call returns false (idempotent guard)", func(t *testing.T) {
		ok, err := q.SetPostProcStarted(j.ID)
		if err != nil {
			t.Fatalf("SetPostProcStarted: %v", err)
		}
		if ok {
			t.Error("expected false on second call")
		}
	})

	t.Run("error on nonexistent job", func(t *testing.T) {
		_, err := q.SetPostProcStarted("bogus")
		if err == nil {
			t.Error("expected error for nonexistent job")
		}
	})
}

// ---------- MarkJobStarted ----------

func TestMarkJobStarted(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "started", constants.NormalPriority)
	_ = q.Add(j)

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("sets start time on first call", func(t *testing.T) {
		if err := q.MarkJobStarted(j.ID, now); err != nil {
			t.Fatalf("MarkJobStarted: %v", err)
		}
		got, _ := q.Get(j.ID)
		if !got.Progress().DownloadStarted().Equal(now) {
			t.Errorf("DownloadStarted = %v, want %v", got.Progress().DownloadStarted(), now)
		}
	})

	t.Run("no-op on subsequent calls", func(t *testing.T) {
		later := now.Add(time.Hour)
		if err := q.MarkJobStarted(j.ID, later); err != nil {
			t.Fatalf("MarkJobStarted: %v", err)
		}
		got, _ := q.Get(j.ID)
		if !got.Progress().DownloadStarted().Equal(now) {
			t.Errorf("DownloadStarted should not change: got %v, want %v", got.Progress().DownloadStarted(), now)
		}
	})

	t.Run("error on nonexistent job", func(t *testing.T) {
		if err := q.MarkJobStarted("bogus", now); !errors.Is(err, ErrNotFound) {
			t.Errorf("got %v, want ErrNotFound", err)
		}
	})
}

// ---------- RecordDownload ----------

func TestRecordDownload(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "download-stats", constants.NormalPriority)
	_ = q.Add(j)

	if err := q.RecordDownload(j.ID, "server-a", 1000); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}
	if err := q.RecordDownload(j.ID, "server-b", 2000); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}
	if err := q.RecordDownload(j.ID, "server-a", 500); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}

	got, _ := q.Get(j.ID)
	if got.Progress().ServerStats()["server-a"] != 1500 {
		t.Errorf("server-a = %d, want 1500", got.Progress().ServerStats()["server-a"])
	}
	if got.Progress().ServerStats()["server-b"] != 2000 {
		t.Errorf("server-b = %d, want 2000", got.Progress().ServerStats()["server-b"])
	}

	if err := q.RecordDownload("bogus", "server-a", 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// ---------- IsComplete ----------

func TestIsComplete(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "complete-check", 3, 2)
	_ = q.Add(j)

	t.Run("not complete initially", func(t *testing.T) {
		got, _ := q.Get(j.ID)
		if got.IsComplete() {
			t.Error("job should not be complete initially")
		}
	})

	t.Run("not complete after partial file completion", func(t *testing.T) {
		_ = q.MarkFileComplete(j.ID, 0)
		_ = q.MarkFileComplete(j.ID, 1)
		got, _ := q.Get(j.ID)
		if got.IsComplete() {
			t.Error("job should not be complete with 2/3 files done")
		}
	})

	t.Run("complete after all files done", func(t *testing.T) {
		_ = q.MarkFileComplete(j.ID, 2)
		got, _ := q.Get(j.ID)
		if !got.IsComplete() {
			t.Error("job should be complete with all 3 files done")
		}
	})
}

// TestIsComplete_WithoutResidentManifest pins that completion is answerable
// from resident state alone.
//
// IsComplete used to return false whenever the manifest was nil, which reads
// as "not complete" and is indistinguishable from a real answer. Application
// startup walks Queue.Snapshot() and finalizes every job reporting complete,
// so a non-resident completed job would be silently skipped and left in the
// queue forever — a wrong answer, not a failure anyone would see.
//
// The file dimension comes from JobProgress's own files slice rather than
// the promoted NumFiles scalar: the loop indexes into that slice, so
// bounding it by the slice's own length keeps the count and the data it
// indexes from ever disagreeing.
func TestIsComplete_WithoutResidentManifest(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "complete-no-manifest", 2, 2)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for fi := range 2 {
		if err := q.MarkFileComplete(j.ID, fi); err != nil {
			t.Fatalf("MarkFileComplete(%d): %v", fi, err)
		}
	}
	if !j.IsComplete() {
		t.Fatal("fixture guard: job is not complete while resident, nothing is being tested")
	}

	// Evict the manifest, exactly as leaving the active set does, keeping
	// progress resident.
	j.setResidency(nil, j.progress)
	if manifestResident(j) {
		t.Fatal("fixture guard: manifest still resident after eviction")
	}

	if !j.IsComplete() {
		t.Error("IsComplete() = false for a completed job whose manifest was evicted; startup finalization would skip it")
	}
}

// A job with no file state cannot report completion either way, and must not
// claim to be complete — an empty loop would satisfy the check vacuously.
//
// This matters because Application startup finalizes every job that reports
// complete. A job row with no job_files rows gets progress sized from an
// empty count slice, so a vacuous true would move an unfinished job into
// history at boot with nothing logged.
func TestIsComplete_AbsentFileStateIsNotComplete(t *testing.T) {
	t.Parallel()

	t.Run("no progress at all", func(t *testing.T) {
		j := &Job{ID: "no-progress"}
		if j.IsComplete() {
			t.Error("IsComplete() = true for a job with no progress; absence of state is not completion")
		}
	})

	t.Run("progress sized to zero files", func(t *testing.T) {
		j := &Job{ID: "zero-files"}
		j.progress = newJobProgressSized(nil)
		if j.progress == nil || len(j.progress.files) != 0 {
			t.Fatal("fixture guard: expected non-nil progress carrying no file state")
		}
		if j.IsComplete() {
			t.Error("IsComplete() = true for a job carrying no file state; startup would finalize it into history")
		}
	})
}

// ---------- SeedFromExtents (batch done, replaces MarkArticlesDone) ----------

func TestSeedFromExtents_Batch(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "batch-done", 1, 4)
	_ = q.Add(j)
	initialRemaining := j.Progress().RemainingBytes() // 400_000

	t.Run("marks multiple articles in one call", func(t *testing.T) {
		ackDone(t, q, j.ID, articleID(0, 0), articleID(0, 2))
		got, _ := q.Get(j.ID)
		if !got.Progress().ArticleDone(0) {
			t.Error("article 0 should be Done")
		}
		if got.Progress().ArticleDone(1) {
			t.Error("article 1 should NOT be Done")
		}
		if !got.Progress().ArticleDone(2) {
			t.Error("article 2 should be Done")
		}
		wantRemaining := initialRemaining - 200_000 // 2 × 100k
		if got.Progress().RemainingBytes() != wantRemaining {
			t.Errorf("RemainingBytes = %d, want %d", got.Progress().RemainingBytes(), wantRemaining)
		}
	})

	t.Run("idempotent: re-marking already-done articles doesn't double-decrement", func(t *testing.T) {
		got, _ := q.Get(j.ID)
		beforeRemaining := got.Progress().RemainingBytes()
		ackDone(t, q, j.ID, articleID(0, 0)) // already done
		got, _ = q.Get(j.ID)
		if got.Progress().RemainingBytes() != beforeRemaining {
			t.Errorf("RemainingBytes changed on re-mark: %d → %d", beforeRemaining, got.Progress().RemainingBytes())
		}
	})

	t.Run("empty batch is no-op", func(t *testing.T) {
		if err := q.SeedFromExtents(j.ID, nil); err != nil {
			t.Errorf("empty batch should not error: %v", err)
		}
	})

	t.Run("error on nonexistent job", func(t *testing.T) {
		exts := []durability.FileExtent{{FileIdx: 0, Durable: durability.NewBitmap(4)}}
		if err := q.SeedFromExtents("bogus", exts); err == nil {
			t.Error("expected error for nonexistent job")
		}
	})
}

// ---------- AckPermanentFailure (batch, replaces MarkArticlesFailed) ----------

func TestAckPermanentFailure_Batch(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "batch-fail", 1, 4)
	_ = q.Add(j)
	initialRemaining := j.Progress().RemainingBytes() // 400_000

	t.Run("marks articles failed and decrements remaining/increments failed bytes", func(t *testing.T) {
		ackFailed(t, q, j.ID, articleID(0, 0), articleID(0, 1))
		got, _ := q.Get(j.ID)
		if got.Progress().FailedBytes() != 200_000 {
			t.Errorf("FailedBytes = %d, want 200000", got.Progress().FailedBytes())
		}
		wantRemaining := initialRemaining - 200_000
		if got.Progress().RemainingBytes() != wantRemaining {
			t.Errorf("RemainingBytes = %d, want %d", got.Progress().RemainingBytes(), wantRemaining)
		}
	})

	t.Run("empty batch is no-op", func(t *testing.T) {
		if err := q.AckPermanentFailure(j.ID, nil); err != nil {
			t.Fatalf("AckPermanentFailure empty: %v", err)
		}
	})
}

// ---------- Full Lifecycle: Emit → Done → FileComplete → IsComplete ----------

func TestFullArticleLifecycle(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "lifecycle", 2, 2)
	_ = q.Add(j)

	// Phase 1: All articles are unfinished.
	count := 0
	q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
		count++
		return true
	})
	if count != 4 {
		t.Fatalf("initial unfinished = %d, want 4", count)
	}

	// Phase 2: Emit all articles (simulating dispatcher sending to workers).
	for i := range mustManifest(t, j).NumArticles() {
		_ = q.MarkArticleEmitted(j.ID, mustManifest(t, j).ArticleID(i))
	}
	count = 0
	q.ForEachUnfinishedArticle(func(_ UnfinishedArticle) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("after emitting all: unfinished = %d, want 0", count)
	}

	// Phase 3: Mark file 0's articles done (simulating assembler completing a file).
	ackDone(t, q, j.ID, articleID(0, 0), articleID(0, 1))
	_ = q.MarkFileComplete(j.ID, 0)

	got, _ := q.Get(j.ID)
	if got.IsComplete() {
		t.Error("should not be complete yet (file 1 pending)")
	}
	if got.Progress().RemainingBytes() != 200_000 {
		t.Errorf("RemainingBytes = %d, want 200000", got.Progress().RemainingBytes())
	}

	// Phase 4: Mark file 1's articles done.
	ackDone(t, q, j.ID, articleID(1, 0), articleID(1, 1))
	_ = q.MarkFileComplete(j.ID, 1)

	got, _ = q.Get(j.ID)
	if !got.IsComplete() {
		t.Error("should be complete after all files done")
	}
	if got.Progress().RemainingBytes() != 0 {
		t.Errorf("RemainingBytes = %d, want 0", got.Progress().RemainingBytes())
	}
}

// ---------- Lifecycle with failures: Emit → Fail → Re-emit → Done ----------

func TestArticleRetryLifecycle(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "retry", 1, 2)
	_ = q.Add(j)
	// Drain notification.
	<-q.Notify()

	msg0 := articleID(0, 0)
	msg1 := articleID(0, 1)

	// Step 1: Dispatcher emits article 0.
	_ = q.MarkArticleEmitted(j.ID, msg0)

	// Step 2: Download fails → clear emitted so it can be retried.
	_ = q.ClearArticleEmitted(j.ID, msg0)

	// Article should be yielded again.
	var yielded []string
	q.ForEachUnfinishedArticle(func(ua UnfinishedArticle) bool {
		yielded = append(yielded, ua.MessageID)
		return true
	})
	if len(yielded) != 2 {
		t.Errorf("expected 2 unfinished after retry clear, got %d", len(yielded))
	}

	// ClearArticleEmitted should have woken the dispatcher.
	select {
	case <-q.Notify():
	case <-time.After(time.Second):
		t.Error("ClearArticleEmitted should signal notify")
	}

	// Step 3: Re-emit and this time it succeeds.
	_ = q.MarkArticleEmitted(j.ID, msg0)
	ackDone(t, q, j.ID, msg0)
	ackDone(t, q, j.ID, msg1)
	_ = q.MarkFileComplete(j.ID, 0)

	got, _ := q.Get(j.ID)
	if !got.IsComplete() {
		t.Error("job should be complete after retry")
	}
	if got.Progress().RemainingBytes() != 0 {
		t.Errorf("RemainingBytes = %d, want 0", got.Progress().RemainingBytes())
	}
}

// ---------- Concurrent article lifecycle ----------

func TestConcurrentArticleLifecycle(t *testing.T) {
	t.Parallel()
	q := New()
	const nFiles = 4
	const nArticles = 10
	j := makeMultiFileJob(t, "concurrent", nFiles, nArticles)
	_ = q.Add(j)

	// Simulate concurrent workers completing articles.
	var wg sync.WaitGroup
	for fi := range nFiles {
		wg.Go(func() {
			for ai := range nArticles {
				msgID := articleID(fi, ai)
				_ = q.MarkArticleEmitted(j.ID, msgID)
				ackDone(t, q, j.ID, msgID)
			}
			_ = q.MarkFileComplete(j.ID, fi)
		})
	}
	wg.Wait()

	got, _ := q.Get(j.ID)
	if !got.IsComplete() {
		t.Error("job should be complete after all goroutines finish")
	}
	if got.Progress().RemainingBytes() != 0 {
		t.Errorf("RemainingBytes = %d, want 0", got.Progress().RemainingBytes())
	}

	// Verify all articles are marked Done.
	m := mustManifest(t, got)
	p := got.Progress()
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			if !p.ArticleDone(i) {
				t.Errorf("file %d, article %d not done", fi, i-lo)
			}
		}
	}
}

// ---------- Duplicate name handling ----------

func TestAddDuplicateNameRenames(t *testing.T) {
	t.Parallel()
	q := New()
	a := makeJob(t, "dupe", constants.NormalPriority)
	b := makeJob(t, "dupe", constants.NormalPriority)
	c := makeJob(t, "dupe", constants.NormalPriority)
	_ = q.Add(a)
	_ = q.Add(b)
	_ = q.Add(c)

	if a.Name != "dupe" {
		t.Errorf("first job name = %q, want %q", a.Name, "dupe")
	}
	if b.Name != "dupe.1" {
		t.Errorf("second job name = %q, want %q", b.Name, "dupe.1")
	}
	if c.Name != "dupe.2" {
		t.Errorf("third job name = %q, want %q", c.Name, "dupe.2")
	}
}

// ---------- Persistence with article state ----------

func TestSaveLoadPreservesArticleState(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "persist", 2, 3)
	_ = q.Add(j)

	// Mark some articles done, some failed, and a file complete. The
	// complete flag goes on file 1, away from the failed article: a
	// complete file's failed bits are dropped on restore (#300), so putting
	// both on file 0 would pin that pre-existing bug instead of the round
	// trip this test is about.
	ackDone(t, q, j.ID, articleID(0, 0))
	ackFailed(t, q, j.ID, articleID(0, 1))
	_ = q.MarkFileComplete(j.ID, 1)
	if err := q.RecordDownload(j.ID, "my-server", 42_000); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}
	if err := q.MarkJobStarted(j.ID, time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}

	// Through the store: the whole-queue JSON engine went in #266 and the
	// per-job document in #298, so this is the only path that writes a live
	// job down. What is under test is one job's state surviving that.
	lj := storeRoundTrip(t, j)

	// Verify article states round-tripped.
	if !lj.Progress().ArticleDone(0) {
		t.Error("article 0,0 should be Done after load")
	}
	if !lj.Progress().ArticleDone(1) || !lj.Progress().ArticleFailed(1) {
		t.Error("article 0,1 should be Done+Failed after load")
	}
	if lj.Progress().ArticleDone(2) {
		t.Error("article 0,2 should NOT be Done after load")
	}

	// Verify file completion.
	if lj.Progress().FileComplete(0) {
		t.Error("file 0 should NOT be Complete after load")
	}
	if !lj.Progress().FileComplete(1) {
		t.Error("file 1 should be Complete after load")
	}

	// Verify Emitted flag is NOT persisted (per B.6 invariant).
	// We emit an article, round-trip, and expect it cleared.
	_ = q.MarkArticleEmitted(j.ID, articleID(1, 0))
	lj2 := storeRoundTrip(t, j)
	if lj2.Progress().ArticleEmitted(3) {
		t.Error("Emitted flag should NOT survive a store round trip (B.6 invariant)")
	}

	// Verify byte accounting.
	if lj.Progress().FailedBytes() != 100_000 {
		t.Errorf("FailedBytes = %d, want 100000", lj.Progress().FailedBytes())
	}
	// ServerStats is deliberately not asserted: it is job-level and the
	// store has never persisted it, so it has never survived a restart.
	// The payload that did carry it was only ever written at finalization,
	// by which point buildHistoryEntry has already folded the per-server
	// totals into the history entry's Meta string.

	// Verify CountUnfinishedArticles works correctly after load. It is a
	// Queue method, so the reloaded job goes into a fresh queue first —
	// which is also what the history-retry path does with LoadJob's result.
	reloaded := New()
	if err := reloaded.Add(lj); err != nil {
		t.Fatalf("Add reloaded job: %v", err)
	}
	count, err := reloaded.CountUnfinishedArticles(j.ID, 0)
	if err != nil {
		t.Fatalf("CountUnfinishedArticles after load: %v", err)
	}
	if count != 1 { // art 0 done, art 1 failed, art 2 unfinished
		t.Errorf("unfinished count after load = %d, want 1", count)
	}
}
