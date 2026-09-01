package queue

import (
	"context"
	"errors"
	"fmt"
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

// articleID builds a fixture Message-ID. It formats the indices as decimal
// rather than offsetting them from '0': the arithmetic form ran past the
// digits for any index above 9, and at 12 and 14 produced '<' and '>' —
// characters internal/nzb refuses, because they close the angle-bracket
// wrapper the ID is interpolated into on the wire. Fixtures with 100
// articles per file were therefore generating Message-IDs that no real NZB
// could contain.
func articleID(fileIdx, artIdx int) string {
	return fmt.Sprintf("f%da%d@test", fileIdx, artIdx)
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

	t.Run("count decreases after a durable ack", func(t *testing.T) {
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

	t.Run("count decreases after a permanent-failure ack", func(t *testing.T) {
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

// ---------- MarkArticleEmittedByIdx / ClearArticleEmittedByIdx / ClearAllEmitted ----------

func TestMarkArticleEmittedByIdx(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "emit", 1, 3)
	_ = q.Add(j)

	msgID := articleID(0, 0)

	t.Run("emitted article is skipped by ForEachUnfinished", func(t *testing.T) {
		if err := q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msgID)); err != nil {
			t.Fatalf("MarkArticleEmittedByIdx: %v", err)
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
		if err := q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msgID)); err != nil {
			t.Errorf("re-emit should not error: %v", err)
		}
	})

	// An out-of-range index is covered by TestArtIdx_EdgeCases; this pins the
	// other error, an absent job, which nothing else in this package covers.
	t.Run("error on nonexistent job", func(t *testing.T) {
		if err := q.MarkArticleEmittedByIdx("bogus", 0); err == nil {
			t.Error("expected error for nonexistent job")
		}
	})
}

func TestClearArticleEmittedByIdx(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "clear-emit", 1, 2)
	_ = q.Add(j)
	// Drain the add notification.
	<-q.Notify()

	msg0 := articleID(0, 0)

	// Emit, then clear.
	_ = q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msg0))
	if err := q.ClearArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msg0)); err != nil {
		t.Fatalf("ClearArticleEmittedByIdx: %v", err)
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

	// ClearArticleEmittedByIdx must signal the dispatcher.
	select {
	case <-q.Notify():
	case <-time.After(time.Second):
		t.Error("ClearArticleEmittedByIdx should signal notify channel")
	}

	// Error cases.
	// An absent job. This is the only pin on ClearArticleEmittedByIdx's
	// ErrNotFound path anywhere in the repo — internal/downloader covers the
	// Mark side, nothing covers this one. An out-of-range index is covered
	// separately by TestArtIdx_EdgeCases.
	if err := q.ClearArticleEmittedByIdx("bogus", 0); err == nil {
		t.Error("expected error for nonexistent job")
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
		_ = q.MarkArticleEmittedByIdx(j1.ID, int32(i)) //nolint:gosec // G115: fixture article counts are tiny
	}
	for i := range mustManifest(t, j2).NumArticles() {
		_ = q.MarkArticleEmittedByIdx(j2.ID, int32(i)) //nolint:gosec // G115: fixture article counts are tiny
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

	q.ClearAllEmitted(nil)

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
		_ = q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, articleID(0, 1)))
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

	t.Run("decreases after a durable ack", func(t *testing.T) {
		if got := q.TotalRemainingBytes(); got != 500_000 {
			t.Errorf("TotalRemainingBytes = %d, want 500000", got)
		}
	})

	// Mark one article failed → remaining also decreases.
	ackFailed(t, q, a.ID, articleID(0, 1))

	t.Run("decreases after a permanent-failure ack", func(t *testing.T) {
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

	// SetPostProcStarted is the second of the two writers that apply the
	// finish transition (TestMarkDownloadFinished names the enumeration), and
	// its write of downloadFinished was untested until #457's review: the two
	// subtests below asserted only the bool and job.PostProc, so deleting its
	// stamp of downloadFinished left them green. #464 replaced that site's
	// literal assignment with a call to the owner,
	// JobProgress.setDownloadFinishedOnce, which is what this test now
	// defends — see testdata/postproc_stamp.spec. firstFinish captures the
	// stamp so the idempotent case can assert the field does not move either.
	var firstFinish time.Time

	t.Run("first call returns true and stamps the finish time", func(t *testing.T) {
		ok, err := q.SetPostProcStarted(j.ID)
		if err != nil {
			t.Fatalf("SetPostProcStarted: %v", err)
		}
		if !ok {
			t.Error("expected true on first call")
		}
		got, _ := q.liveJob(j.ID)
		firstFinish = got.Progress().DownloadFinished()
		if firstFinish.IsZero() {
			t.Error("DownloadFinished still zero; SetPostProcStarted did not stamp the finish time, " +
				"which feeds the history record's elapsed figure")
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
		// The field not moving here is the job.PostProc early return, NOT the
		// first-wins guard — that return fires before the stamp is reached, so
		// neutering the guard in the owner leaves this subtest green.
		// Mutation-checked. The case the guard actually covers is the subtest
		// below, which is where testdata/postproc_stamp.spec lands its kill.
		got, _ := q.liveJob(j.ID)
		if f := got.Progress().DownloadFinished(); !f.Equal(firstFinish) {
			t.Errorf("DownloadFinished moved on the second call: got %v, want %v", f, firstFinish)
		}
	})

	// What the first-wins guard is for: a job whose finish time was already
	// set by Queue.MarkDownloadFinished, reaching SetPostProcStarted for the
	// first time. PostProc is still false, so the early return above does not
	// fire and the guard is the only thing standing between the two writers.
	// Since #464 both of them reach it through the same owner method, so the
	// guard is one implementation rather than two that agreed by inspection.
	t.Run("does not overwrite a finish time MarkDownloadFinished already set", func(t *testing.T) {
		q2 := New()
		j2 := makeJob(t, "postproc-preset", constants.NormalPriority)
		if err := q2.Add(j2); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := q2.SetStatus(j2.ID, constants.StatusDownloading); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		marked := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
		if err := q2.MarkDownloadFinished(j2.ID, marked); err != nil {
			t.Fatalf("MarkDownloadFinished: %v", err)
		}
		if _, err := q2.SetPostProcStarted(j2.ID); err != nil {
			t.Fatalf("SetPostProcStarted: %v", err)
		}
		got, _ := q2.liveJob(j2.ID)
		if f := got.Progress().DownloadFinished(); !f.Equal(marked) {
			t.Errorf("DownloadFinished = %v, want %v — SetPostProcStarted overwrote a finish "+
				"time another writer had already set, moving the job's reported duration", f, marked)
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
		got, _ := q.liveJob(j.ID)
		if !got.Progress().DownloadStarted().Equal(now) {
			t.Errorf("DownloadStarted = %v, want %v", got.Progress().DownloadStarted(), now)
		}
	})

	t.Run("no-op on subsequent calls", func(t *testing.T) {
		later := now.Add(time.Hour)
		if err := q.MarkJobStarted(j.ID, later); err != nil {
			t.Fatalf("MarkJobStarted: %v", err)
		}
		got, _ := q.liveJob(j.ID)
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

// TestMarkDownloadFinished mirrors TestMarkJobStarted for the finish
// timestamp, whose first-wins rule had no test at all.
//
// The gap was found by mutation during B2.4a: neutering the IsZero guard in
// markDownloadFinishedOnce left the whole suite green, while the same
// mutation on markStartedOnce was caught immediately by the sibling above.
// Before this, the only test touching Queue.MarkDownloadFinished was a
// concurrency challenger that discards the error and never re-marks.
//
// It matters beyond symmetry. DownloadFinished feeds the history record's
// elapsed time, so a second write moves a completed job's reported duration.
// The untested rule is also why #457 went unnoticed: Job carried an exported
// MarkDownloadFinished that assigned unconditionally, and nothing asserted the
// two should agree. #457 deleted it rather than guarding it.
//
// #464 settled the writer set: both download timestamps have one owner in
// progress.go, and `git grep -nE 'downloadFinished[[:space:]]*=' -- '*.go'
// ':!*_test.go'` returns 3 lines, all of them inside it. The count was 5
// before that change and 8 midway through it, which is why this citation spent
// several commits as prose — a number stated then would have been wrong at
// three of them, and check_citations would have failed rather than merely read
// stale. The population is now pinned by
// TestDownloadStampWriters_MatchTheEnumerationStatedInProse, which fails when
// the set moves; this count is orientation, not the guarantee.
//
// What does not change is the claim this comment exists to make: the two
// callers that APPLY the finish transition are markDownloadFinishedOnce and
// SetPostProcStarted, so this test and TestSetPostProcStarted between them
// cover the transition class.
// That was not true when first written: TestSetPostProcStarted asserted only
// its bool and job.PostProc, so deleting its stamp of downloadFinished left
// it green. #457's review caught the overclaim and the assertions were added
// rather than the sentence weakened.
func TestMarkDownloadFinished(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "finished", constants.NormalPriority)
	_ = q.Add(j)

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("sets finish time on first call", func(t *testing.T) {
		if err := q.MarkDownloadFinished(j.ID, now); err != nil {
			t.Fatalf("MarkDownloadFinished: %v", err)
		}
		got, _ := q.liveJob(j.ID)
		if !got.Progress().DownloadFinished().Equal(now) {
			t.Errorf("DownloadFinished = %v, want %v", got.Progress().DownloadFinished(), now)
		}
	})

	t.Run("no-op on subsequent calls", func(t *testing.T) {
		later := now.Add(time.Hour)
		if err := q.MarkDownloadFinished(j.ID, later); err != nil {
			t.Fatalf("MarkDownloadFinished: %v", err)
		}
		got, _ := q.liveJob(j.ID)
		if !got.Progress().DownloadFinished().Equal(now) {
			t.Errorf("DownloadFinished should not change: got %v, want %v — first finish wins, "+
				"and a later write would move a completed job's reported duration",
				got.Progress().DownloadFinished(), now)
		}
	})

	t.Run("error on nonexistent job", func(t *testing.T) {
		if err := q.MarkDownloadFinished("bogus", now); !errors.Is(err, ErrNotFound) {
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

	got, _ := q.liveJob(j.ID)
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
		got, _ := q.liveJob(j.ID)
		if got.IsComplete() {
			t.Error("job should not be complete initially")
		}
	})

	t.Run("not complete after partial file completion", func(t *testing.T) {
		_ = q.MarkFileComplete(j.ID, 0)
		_ = q.MarkFileComplete(j.ID, 1)
		got, _ := q.liveJob(j.ID)
		if got.IsComplete() {
			t.Error("job should not be complete with 2/3 files done")
		}
	})

	t.Run("complete after all files done", func(t *testing.T) {
		_ = q.MarkFileComplete(j.ID, 2)
		got, _ := q.liveJob(j.ID)
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

// ---------- SeedFromRuns (batch done, replaces MarkArticlesDone) ----------

func TestSeedFromRuns_Batch(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeMultiFileJob(t, "batch-done", 1, 4)
	_ = q.Add(j)
	initialRemaining := j.Progress().RemainingBytes() // 400_000

	t.Run("marks multiple articles in one call", func(t *testing.T) {
		ackDone(t, q, j.ID, articleID(0, 0), articleID(0, 2))
		got, _ := q.liveJob(j.ID)
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
		got, _ := q.liveJob(j.ID)
		beforeRemaining := got.Progress().RemainingBytes()
		ackDone(t, q, j.ID, articleID(0, 0)) // already done
		got, _ = q.liveJob(j.ID)
		if got.Progress().RemainingBytes() != beforeRemaining {
			t.Errorf("RemainingBytes changed on re-mark: %d → %d", beforeRemaining, got.Progress().RemainingBytes())
		}
	})

	t.Run("empty batch is no-op", func(t *testing.T) {
		if err := q.SeedFromRuns(j.ID, nil); err != nil {
			t.Errorf("empty batch should not error: %v", err)
		}
	})

	t.Run("error on nonexistent job", func(t *testing.T) {
		runs := []durability.Run{{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Length: 1}}
		if err := q.SeedFromRuns("bogus", runs); err == nil {
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
		got, _ := q.liveJob(j.ID)
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
		_ = q.MarkArticleEmittedByIdx(j.ID, int32(i)) //nolint:gosec // G115: fixture article counts are tiny
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

	got, _ := q.liveJob(j.ID)
	if got.IsComplete() {
		t.Error("should not be complete yet (file 1 pending)")
	}
	if got.Progress().RemainingBytes() != 200_000 {
		t.Errorf("RemainingBytes = %d, want 200000", got.Progress().RemainingBytes())
	}

	// Phase 4: Mark file 1's articles done.
	ackDone(t, q, j.ID, articleID(1, 0), articleID(1, 1))
	_ = q.MarkFileComplete(j.ID, 1)

	got, _ = q.liveJob(j.ID)
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
	_ = q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msg0))

	// Step 2: Download fails → clear emitted so it can be retried.
	_ = q.ClearArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msg0))

	// Article should be yielded again.
	var yielded []string
	q.ForEachUnfinishedArticle(func(ua UnfinishedArticle) bool {
		yielded = append(yielded, ua.MessageID)
		return true
	})
	if len(yielded) != 2 {
		t.Errorf("expected 2 unfinished after retry clear, got %d", len(yielded))
	}

	// ClearArticleEmittedByIdx should have woken the dispatcher.
	select {
	case <-q.Notify():
	case <-time.After(time.Second):
		t.Error("ClearArticleEmittedByIdx should signal notify")
	}

	// Step 3: Re-emit and this time it succeeds.
	_ = q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msg0))
	ackDone(t, q, j.ID, msg0)
	ackDone(t, q, j.ID, msg1)
	_ = q.MarkFileComplete(j.ID, 0)

	got, _ := q.liveJob(j.ID)
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
				_ = q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, msgID))
				ackDone(t, q, j.ID, msgID)
			}
			_ = q.MarkFileComplete(j.ID, fi)
		})
	}
	wg.Wait()

	got, _ := q.liveJob(j.ID)
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
	_ = q.MarkArticleEmittedByIdx(j.ID, artIdxFor(t, q, j.ID, articleID(1, 0)))
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

// updateCountingStore counts Update calls so a test can assert that a mark
// which reported no change did not reach persistence.
type updateCountingStore struct {
	Store
	updates int
}

func (s *updateCountingStore) Update(_ context.Context, _ *Job) error {
	s.updates++
	return nil
}

// TestMarkTimestamp_ZeroIsRefusedAtTheQueueWrapper covers what
// TestJobMarkOnce_RefusesAStampTheStoreCannotDistinguish cannot see.
//
// That test calls the unexported *Job methods on a standalone struct, so it
// pins the bool and the field but not the consequences the bool exists to
// control. Those consequences are the whole argument for #459 — a zero time
// that reported success made the wrapper dirty the queue and run store.Update
// for a write that did not happen — and they live in Queue.MarkJobStarted and
// Queue.MarkDownloadFinished, which branch on the bool.
//
// So this asserts at the wrapper: no store write, no dirty flag, and the
// first-wins slot still open for a real timestamp afterwards.
func TestMarkTimestamp_ZeroIsRefusedAtTheQueueWrapper(t *testing.T) {
	t.Parallel()

	real := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		mark  func(q *Queue, id string, t time.Time) error
		field func(j *Job) time.Time
	}{
		{"MarkJobStarted",
			func(q *Queue, id string, t time.Time) error { return q.MarkJobStarted(id, t) },
			func(j *Job) time.Time { return j.Progress().DownloadStarted() }},
		{"MarkDownloadFinished",
			func(q *Queue, id string, t time.Time) error { return q.MarkDownloadFinished(id, t) },
			func(j *Job) time.Time { return j.Progress().DownloadFinished() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Add before the store is attached, so the only Update the probe
			// can see comes from the marks below.
			q := New()
			j := makeJob(t, "zero-"+tc.name, constants.NormalPriority)
			if err := q.Add(j); err != nil {
				t.Fatalf("Add: %v", err)
			}
			probe := &updateCountingStore{}
			q.store = probe
			q.dirty.Store(false)

			if err := tc.mark(q, j.ID, time.Time{}); err != nil {
				t.Fatalf("marking with a zero time: %v", err)
			}
			if probe.updates != 0 {
				t.Errorf("store.Update ran %d time(s) for a refused zero mark, want 0", probe.updates)
			}
			if q.IsDirty() {
				t.Error("queue went dirty for a refused zero mark")
			}
			got, _ := q.liveJob(j.ID)
			if f := tc.field(got); !f.IsZero() {
				t.Errorf("field = %v after a refused zero mark, want the zero time", f)
			}

			// The refusal must leave the first-wins slot open, and the real
			// mark must then do the persistence the zero one did not.
			if err := tc.mark(q, j.ID, real); err != nil {
				t.Fatalf("marking with a real time: %v", err)
			}
			if probe.updates != 1 {
				t.Errorf("store.Update ran %d time(s) for the real mark, want 1", probe.updates)
			}
			if !q.IsDirty() {
				t.Error("queue stayed clean after a real mark")
			}
			got, _ = q.liveJob(j.ID)
			if f := tc.field(got); !f.Equal(real) {
				t.Errorf("field = %v, want %v — the refusal consumed the first-wins slot", f, real)
			}
		})
	}
}
