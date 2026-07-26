package queue

import (
	"errors"
	"testing"

	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// ---------- SetStatusIf ----------

func TestSetStatusIf(t *testing.T) {
	t.Parallel()

	t.Run("transitions when current matches", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeJob(t, "ssi-match", constants.NormalPriority)
		_ = q.Add(j)

		// Job starts as Queued; transition Queued → Downloading.
		if err := q.SetStatusIf(j.ID, constants.StatusDownloading, constants.StatusQueued); err != nil {
			t.Fatalf("SetStatusIf: %v", err)
		}
		got, _ := q.Get(j.ID)
		if got.Status != constants.StatusDownloading {
			t.Errorf("Status = %q, want %q", got.Status, constants.StatusDownloading)
		}
		if !q.IsDirty() {
			t.Error("expected dirty flag to be set after successful transition")
		}
	})

	t.Run("no-op when current does not match", func(t *testing.T) {
		t.Parallel()
		q := New()
		q.PauseAll()
		j := makeJob(t, "ssi-mismatch", constants.NormalPriority)
		_ = q.Add(j)
		// Save to clear dirty flag.
		dir := t.TempDir()
		_ = q.Save(dir)

		// Job is Queued; try to transition from Paused → Downloading.
		if err := q.SetStatusIf(j.ID, constants.StatusDownloading, constants.StatusPaused); err != nil {
			t.Fatalf("SetStatusIf should not error on mismatch: %v", err)
		}
		got, _ := q.Get(j.ID)
		if got.Status != constants.StatusQueued {
			t.Errorf("Status should remain Queued, got %q", got.Status)
		}
		if q.IsDirty() {
			t.Error("dirty flag should not be set when condition didn't match")
		}
	})

	t.Run("returns ErrNotFound for unknown job", func(t *testing.T) {
		t.Parallel()
		q := New()
		err := q.SetStatusIf("nonexistent", constants.StatusDownloading, constants.StatusQueued)
		if err == nil {
			t.Fatal("expected error for nonexistent job")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("chained transitions", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeJob(t, "ssi-chain", constants.NormalPriority)
		_ = q.Add(j)

		// Queued → Downloading (succeeds)
		_ = q.SetStatusIf(j.ID, constants.StatusDownloading, constants.StatusQueued)
		// Downloading → Verifying (succeeds)
		_ = q.SetStatusIf(j.ID, constants.StatusVerifying, constants.StatusDownloading)
		// Downloading → Repairing (no-op: status is now Verifying, not Downloading)
		_ = q.SetStatusIf(j.ID, constants.StatusRepairing, constants.StatusDownloading)

		got, _ := q.Get(j.ID)
		if got.Status != constants.StatusVerifying {
			t.Errorf("Status = %q, want Verifying after failed chain step", got.Status)
		}
	})
}

// ---------- HasDownloadableJobs ----------

func TestHasDownloadableJobs(t *testing.T) {
	t.Parallel()

	t.Run("empty queue", func(t *testing.T) {
		t.Parallel()
		q := New()
		if q.HasDownloadableJobs() {
			t.Error("empty queue should not have downloadable jobs")
		}
	})

	t.Run("single queued job", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeJob(t, "dl-queued", constants.NormalPriority)
		_ = q.Add(j)
		if !q.HasDownloadableJobs() {
			t.Error("queue with a Queued job should have downloadable jobs")
		}
	})

	t.Run("single downloading job", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeJob(t, "dl-active", constants.NormalPriority)
		_ = q.Add(j)
		_ = q.SetStatus(j.ID, constants.StatusDownloading)
		if !q.HasDownloadableJobs() {
			t.Error("queue with a Downloading job should have downloadable jobs")
		}
	})

	t.Run("only paused jobs", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeJob(t, "dl-paused", constants.NormalPriority)
		_ = q.Add(j)
		_ = q.Pause(j.ID)
		if q.HasDownloadableJobs() {
			t.Error("queue with only paused jobs should not have downloadable jobs")
		}
	})

	t.Run("only post-processing jobs", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeJob(t, "dl-pp", constants.NormalPriority)
		_ = q.Add(j)
		_ = q.SetStatus(j.ID, constants.StatusDownloading)
		ok, err := q.SetPostProcStarted(j.ID)
		if err != nil || !ok {
			t.Fatalf("SetPostProcStarted: %v, %v", ok, err)
		}
		if q.HasDownloadableJobs() {
			t.Error("queue with only post-processing jobs should not have downloadable jobs")
		}
	})

	t.Run("mix of paused and downloadable", func(t *testing.T) {
		t.Parallel()
		q := New()
		j1 := makeJob(t, "dl-mix1", constants.NormalPriority)
		j2 := makeJob(t, "dl-mix2", constants.NormalPriority)
		_ = q.Add(j1)
		_ = q.Add(j2)
		_ = q.Pause(j1.ID)
		if !q.HasDownloadableJobs() {
			t.Error("queue with one paused and one queued job should have downloadable jobs")
		}
	})

	t.Run("all paused and post-processing", func(t *testing.T) {
		t.Parallel()
		q := New()
		j1 := makeJob(t, "dl-allp1", constants.NormalPriority)
		j2 := makeJob(t, "dl-allp2", constants.NormalPriority)
		_ = q.Add(j1)
		_ = q.Add(j2)
		_ = q.Pause(j1.ID)
		_ = q.SetStatus(j2.ID, constants.StatusDownloading)
		ok, err := q.SetPostProcStarted(j2.ID)
		if err != nil || !ok {
			t.Fatalf("SetPostProcStarted: %v, %v", ok, err)
		}
		if q.HasDownloadableJobs() {
			t.Error("queue with all paused/post-proc jobs should not have downloadable jobs")
		}
	})
}

// ---------- CheckEarlyAbort ----------

func TestCheckEarlyAbort(t *testing.T) {
	t.Parallel()

	// makeAbortJob creates a job with enough articles to trigger early abort checks.
	// We use makeMultiFileJob from lifecycle_test.go via the same pattern.
	makeAbortJob := func(t *testing.T, name string, nArticles int) *Job {
		t.Helper()
		parsed := &nzb.NZB{
			Meta:   map[string][]string{"title": {name}},
			Groups: []string{"alt.binaries.test"},
			AvgAge: time.Unix(1700000000, 0),
		}
		f := nzb.File{
			Subject: name + " - data.rar",
			Date:    time.Unix(1700000000, 0),
		}
		for ai := range nArticles {
			art := nzb.Article{
				ID:     name + "-art" + string(rune('0'+ai)) + "@test",
				Bytes:  100_000,
				Number: ai + 1,
			}
			f.Articles = append(f.Articles, art)
			f.Bytes += int64(art.Bytes)
		}
		parsed.Files = append(parsed.Files, f)
		job, err := NewJob(parsed, AddOptions{
			Filename: name + ".nzb",
			Priority: constants.NormalPriority,
		}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		return job
	}

	t.Run("nonexistent job returns false", func(t *testing.T) {
		t.Parallel()
		q := New()
		if q.CheckEarlyAbort("nonexistent") {
			t.Error("should return false for nonexistent job")
		}
	})

	t.Run("not enough resolved articles returns false", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeAbortJob(t, "ea-low", 20)
		_ = q.Add(j)
		// Fail only 5 articles (below the 10-article sample threshold).
		for i := range 5 {
			msgID := j.Manifest().ArticleID(i)
			_, _ = q.MarkArticleFailed(j.ID, msgID)
		}
		if q.CheckEarlyAbort(j.ID) {
			t.Error("should return false when fewer than earlyAbortSample articles resolved")
		}
	})

	t.Run("above threshold triggers early abort", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeAbortJob(t, "ea-trigger", 20)
		_ = q.Add(j)
		// Fail 9 out of 10 articles → 90% failure rate (above 80% threshold).
		for i := range 9 {
			msgID := j.Manifest().ArticleID(i)
			_, _ = q.MarkArticleFailed(j.ID, msgID)
		}
		// Succeed 1 article to reach 10 resolved.
		_ = q.MarkArticleDone(j.ID, j.Manifest().ArticleID(9))

		if !q.CheckEarlyAbort(j.ID) {
			t.Error("should return true when failure rate exceeds threshold")
		}
	})

	t.Run("fires only once", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeAbortJob(t, "ea-once", 20)
		_ = q.Add(j)
		// Fail 10 out of 10 articles → 100% failure rate.
		for i := range 10 {
			msgID := j.Manifest().ArticleID(i)
			_, _ = q.MarkArticleFailed(j.ID, msgID)
		}

		first := q.CheckEarlyAbort(j.ID)
		second := q.CheckEarlyAbort(j.ID)
		if !first {
			t.Error("first CheckEarlyAbort should return true")
		}
		if second {
			t.Error("second CheckEarlyAbort should return false (already fired)")
		}
	})

	t.Run("below threshold does not trigger", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeAbortJob(t, "ea-below", 20)
		_ = q.Add(j)
		// Fail 7 out of 10 → 70% (below 80% threshold).
		for i := range 7 {
			msgID := j.Manifest().ArticleID(i)
			_, _ = q.MarkArticleFailed(j.ID, msgID)
		}
		// Succeed 3 articles.
		for i := 7; i < 10; i++ {
			_ = q.MarkArticleDone(j.ID, j.Manifest().ArticleID(i))
		}

		if q.CheckEarlyAbort(j.ID) {
			t.Error("should return false when failure rate is below threshold")
		}
	})
}

// ---------- SetFileCRC32 ----------

func TestSetFileCRC32(t *testing.T) {
	t.Parallel()

	t.Run("sets CRC on valid file index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-valid", 2, 2)
		_ = q.Add(j)
		dir := t.TempDir()
		_ = q.Save(dir)

		var crc uint32 = 0xDEADBEEF
		if err := q.SetFileCRC32(j.ID, 0, crc); err != nil {
			t.Fatalf("SetFileCRC32: %v", err)
		}
		got, _ := q.Get(j.ID)
		if got.Progress().FileAssembledCRC32(0) != crc {
			t.Errorf("AssembledCRC32 = 0x%X, want 0x%X", got.Progress().FileAssembledCRC32(0), crc)
		}
		// File 1 should be unaffected.
		if got.Progress().FileAssembledCRC32(1) != 0 {
			t.Errorf("File 1 AssembledCRC32 = 0x%X, want 0", got.Progress().FileAssembledCRC32(1))
		}
		if !q.IsDirty() {
			t.Error("SetFileCRC32 should set dirty flag")
		}
	})

	t.Run("sets CRC on second file", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-second", 3, 1)
		_ = q.Add(j)

		var crc uint32 = 0xCAFEBABE
		if err := q.SetFileCRC32(j.ID, 2, crc); err != nil {
			t.Fatalf("SetFileCRC32: %v", err)
		}
		got, _ := q.Get(j.ID)
		if got.Progress().FileAssembledCRC32(2) != crc {
			t.Errorf("AssembledCRC32 = 0x%X, want 0x%X", got.Progress().FileAssembledCRC32(2), crc)
		}
	})

	t.Run("error on out-of-bounds index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-oob", 1, 1)
		_ = q.Add(j)

		if err := q.SetFileCRC32(j.ID, j.Manifest().NumFiles(), 0x1234); err == nil {
			t.Error("expected error for out-of-bounds file index")
		}
	})

	t.Run("error on negative index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-neg", 1, 1)
		_ = q.Add(j)

		if err := q.SetFileCRC32(j.ID, -1, 0x1234); err == nil {
			t.Error("expected error for negative file index")
		}
	})

	t.Run("error on unknown job", func(t *testing.T) {
		t.Parallel()
		q := New()
		err := q.SetFileCRC32("nonexistent", 0, 0x1234)
		if err == nil {
			t.Fatal("expected error for nonexistent job")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})
}

// ---------- SetFileFilename ----------

func TestSetFileFilename(t *testing.T) {
	t.Parallel()

	t.Run("sets filename on valid file index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "filename-valid", 2, 2)
		_ = q.Add(j)
		dir := t.TempDir()
		_ = q.Save(dir)

		filename := "resolved.mkv"
		if err := q.SetFileFilename(j.ID, 0, filename); err != nil {
			t.Fatalf("SetFileFilename: %v", err)
		}
		got, _ := q.Get(j.ID)
		if got.Progress().FileFilename(0) != filename {
			t.Errorf("Filename = %q, want %q", got.Progress().FileFilename(0), filename)
		}
		// File 1 should be unaffected.
		if got.Progress().FileFilename(1) != "" {
			t.Errorf("File 1 Filename = %q, want empty", got.Progress().FileFilename(1))
		}
		if !q.IsDirty() {
			t.Error("SetFileFilename should set dirty flag")
		}
	})

	t.Run("error on out-of-bounds index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "filename-oob", 1, 1)
		_ = q.Add(j)

		if err := q.SetFileFilename(j.ID, j.Manifest().NumFiles(), "bad.mkv"); err == nil {
			t.Error("expected error for out-of-bounds file index")
		}
	})

	t.Run("error on unknown job", func(t *testing.T) {
		t.Parallel()
		q := New()
		err := q.SetFileFilename("nonexistent", 0, "bad.mkv")
		if err == nil {
			t.Fatal("expected error for nonexistent job")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})
}

// ---------- SnapshotJob / SnapshotJobByName ----------

func TestSnapshotJob_Audit(t *testing.T) {
	t.Parallel()

	t.Run("returns deep copy of existing job", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeJob(t, "snap-exist", constants.NormalPriority)
		_ = q.Add(j)

		snap := q.SnapshotJob(j.ID)
		if snap == nil {
			t.Fatal("SnapshotJob returned nil for existing job")
		}
		if snap.ID != j.ID {
			t.Errorf("ID = %q, want %q", snap.ID, j.ID)
		}
		if snap.Name != j.Name {
			t.Errorf("Name = %q, want %q", snap.Name, j.Name)
		}
		// Mutating the snapshot must not affect the original.
		snap.Status = constants.StatusFailed
		got, _ := q.Get(j.ID)
		if got.Status == constants.StatusFailed {
			t.Error("mutation of snapshot leaked to original")
		}
	})

	t.Run("returns nil for nonexistent job", func(t *testing.T) {
		t.Parallel()
		q := New()
		if snap := q.SnapshotJob("nonexistent"); snap != nil {
			t.Errorf("expected nil, got %+v", snap)
		}
	})
}
