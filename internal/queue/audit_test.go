package queue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
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
				ID:     fmt.Sprintf("%s-art%d@test", name, ai),
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
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
		// Fail only 5 articles (below the 10-article sample threshold).
		for i := range 5 {
			msgID := mustManifest(t, j).ArticleID(i)
			ackFailed(t, q, j.ID, msgID)
		}
		if q.CheckEarlyAbort(j.ID) {
			t.Error("should return false when fewer than earlyAbortSample articles resolved")
		}
	})

	t.Run("above threshold triggers early abort", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeAbortJob(t, "ea-trigger", 20)
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
		// Fail 9 out of 10 articles → 90% failure rate (above 80% threshold).
		for i := range 9 {
			msgID := mustManifest(t, j).ArticleID(i)
			ackFailed(t, q, j.ID, msgID)
		}
		// Succeed 1 article to reach 10 resolved.
		ackDone(t, q, j.ID, mustManifest(t, j).ArticleID(9))

		if !q.CheckEarlyAbort(j.ID) {
			t.Error("should return true when failure rate exceeds threshold")
		}
	})

	t.Run("fires only once", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeAbortJob(t, "ea-once", 20)
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
		// Fail 10 out of 10 articles → 100% failure rate.
		for i := range 10 {
			msgID := mustManifest(t, j).ArticleID(i)
			ackFailed(t, q, j.ID, msgID)
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
		if err := q.Add(j); err != nil {
			t.Fatalf("Add: %v", err)
		}
		// Fail 7 out of 10 → 70% (below 80% threshold).
		for i := range 7 {
			msgID := mustManifest(t, j).ArticleID(i)
			ackFailed(t, q, j.ID, msgID)
		}
		// Succeed 3 articles.
		for i := 7; i < 10; i++ {
			ackDone(t, q, j.ID, mustManifest(t, j).ArticleID(i))
		}

		if q.CheckEarlyAbort(j.ID) {
			t.Error("should return false when failure rate is below threshold")
		}
	})
}

// ---------- SetFileCRC32FromRuns ----------

// coveringRuns builds the record a fully-accounted file has: ONE run at offset
// 0 whose article span is the file's whole range.
//
// Tests that merely need a CRC on a file go through this rather than writing
// the field, because the gatekeeper's whole point is that the value and its
// evidence arrive together — a fixture that could set a CRC no record could
// produce would be testing a state the program cannot reach.
func coveringRuns(t *testing.T, m *Manifest, fileIdx int, crc uint32) []durability.Run {
	t.Helper()
	lo, hi := m.FileRange(fileIdx)
	return []durability.Run{{
		FileIdx:     int32(fileIdx),
		FirstArtIdx: int32(lo),
		LastArtIdx:  int32(hi - 1),
		Offset:      0,
		Length:      m.FileBytes(fileIdx),
		CRC32:       crc,
	}}
}

func TestSetFileCRC32FromRuns(t *testing.T) {
	t.Parallel()

	t.Run("sets CRC on valid file index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-valid", 2, 2)
		_ = q.Add(j)
		dir := t.TempDir()
		_ = q.Save(dir)

		var crc uint32 = 0xDEADBEEF
		if err := q.SetFileCRC32FromRuns(j.ID, 0, coveringRuns(t, mustManifest(t, j), 0, crc)); err != nil {
			t.Fatalf("SetFileCRC32FromRuns: %v", err)
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
			t.Error("SetFileCRC32FromRuns should set dirty flag")
		}
	})

	t.Run("sets CRC on second file", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-second", 3, 1)
		_ = q.Add(j)

		var crc uint32 = 0xCAFEBABE
		if err := q.SetFileCRC32FromRuns(j.ID, 2, coveringRuns(t, mustManifest(t, j), 2, crc)); err != nil {
			t.Fatalf("SetFileCRC32FromRuns: %v", err)
		}
		got, _ := q.Get(j.ID)
		if got.Progress().FileAssembledCRC32(2) != crc {
			t.Errorf("AssembledCRC32 = 0x%X, want 0x%X", got.Progress().FileAssembledCRC32(2), crc)
		}
	})

	// The three withholding cases below are the reason this method takes runs
	// at all. Each is a record that does NOT account for the whole file, and
	// each must leave the field at zero — which QuickCheck reads as NoCRC and
	// routes to par2, rather than as a verified match.
	withheld := []struct {
		name  string
		runs  func(m *Manifest) []durability.Run
		why   string
		crc   uint32
		files int
		arts  int
	}{
		{
			name:  "an article missing from the span",
			files: 1, arts: 3, crc: 0xDEADBEEF,
			runs: func(m *Manifest) []durability.Run {
				r := coveringRuns(t, m, 0, 0xDEADBEEF)
				r[0].LastArtIdx-- // one article unaccounted for
				return r
			},
			why: "this is the exact-offset collision's signature — the dropped " +
				"article's index is in no run's span — and it is also every " +
				"permanently failed article. Publishing here is #387: the value " +
				"is a real CRC over the articles the record DOES hold, so it can " +
				"match par2's manifest while the file on disk is not those bytes",
		},
		{
			name:  "more than one run",
			files: 1, arts: 3, crc: 0xDEADBEEF,
			runs: func(m *Manifest) []durability.Run {
				lo, hi := m.FileRange(0)
				return []durability.Run{
					{FileIdx: 0, FirstArtIdx: int32(lo), LastArtIdx: int32(lo), Offset: 0, Length: 10, CRC32: 0xDEADBEEF},
					{FileIdx: 0, FirstArtIdx: int32(hi - 1), LastArtIdx: int32(hi - 1), Offset: 500, Length: 10, CRC32: 0xBEEF},
				}
			},
			why: "two rows means a gap between them, so no single CRC describes " +
				"the file and the first row's value describes only its prefix",
		},
		{
			name:  "a single run that does not start at zero",
			files: 1, arts: 3, crc: 0xDEADBEEF,
			runs: func(m *Manifest) []durability.Run {
				r := coveringRuns(t, m, 0, 0xDEADBEEF)
				r[0].Offset = 10
				return r
			},
			why: "the file's first bytes are accounted for by nothing, so a CRC " +
				"folded from this run describes a suffix, not the file",
		},
	}
	for _, tc := range withheld {
		t.Run("withholds: "+tc.name, func(t *testing.T) {
			t.Parallel()
			q := New()
			j := makeMultiFileJob(t, "crc-"+tc.name, tc.files, tc.arts)
			_ = q.Add(j)

			if err := q.SetFileCRC32FromRuns(j.ID, 0, tc.runs(mustManifest(t, j))); err != nil {
				t.Fatalf("SetFileCRC32FromRuns: %v — withholding is not an error, "+
					"it is the ordinary answer for an incomplete record", err)
			}
			got, _ := q.Get(j.ID)
			if c := got.Progress().FileAssembledCRC32(0); c != 0 {
				t.Errorf("AssembledCRC32 = 0x%X, want 0 (withheld): %s", c, tc.why)
			}
		})
	}

	t.Run("error on out-of-bounds index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-oob", 1, 1)
		_ = q.Add(j)

		n := mustManifest(t, j).NumFiles()
		if err := q.SetFileCRC32FromRuns(j.ID, n, nil); err == nil {
			t.Error("expected error for out-of-bounds file index")
		}
	})

	t.Run("error on negative index", func(t *testing.T) {
		t.Parallel()
		q := New()
		j := makeMultiFileJob(t, "crc-neg", 1, 1)
		_ = q.Add(j)

		if err := q.SetFileCRC32FromRuns(j.ID, -1, nil); err == nil {
			t.Error("expected error for negative file index")
		}
	})

	t.Run("error on unknown job", func(t *testing.T) {
		t.Parallel()
		q := New()
		err := q.SetFileCRC32FromRuns("nonexistent", 0, nil)
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

		if err := q.SetFileFilename(j.ID, mustManifest(t, j).NumFiles(), "bad.mkv"); err == nil {
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

// TestCheckEarlyAbort_NonResidentDefersRatherThanAborts pins the two halves of
// why CheckEarlyAbort's residency gate is correct — which is the opposite of
// what #461 concluded, and the reason that issue closed as not-a-defect.
//
// #461's premise was that the gate refused an answer the method could always
// give, costing "a DMCA'd or expired job continuing to burn bandwidth". Both
// clauses are wrong. IsResident is PhaseActive or PhaseProcessing, so a
// non-resident job is Queued/Propagating/Paused/terminal and is not
// downloading; and no legal status edge runs from any of those to
// StatusVerifying, so the abort's only consumer — pipeline.go's onJobHopeless
// → maybeFinalize → SetPostProcStarted — fails with
// ErrIllegalStatusTransition and drops the verdict silently. Ungating it
// produced a WARN saying a job had been aborted while nothing aborted it.
//
// So the two halves:
//
//   - GATED: a non-resident job at a failure rate well past the threshold
//     still answers false.
//   - DEFERRED, NOT DISCARDED: once the job is downloading again the same
//     rate fires the abort. PromoteNext rebuilds JobProgress
//     (newJobProgress + the store's persisted counters) and earlyAborted is
//     deliberately not persisted, so nothing carries a spent latch across.
//
// The second half is what makes the first safe, and it is the half a reader
// cannot get from the gate alone — which is how #461 came to be filed.
//
// Store-backed on purpose: without one, PromoteNext's rehydration derives
// counters from the manifest alone, the 90% rate does not survive the
// round-trip, and the deferral half would pass for the wrong reason.
func TestCheckEarlyAbort_NonResidentDefersRatherThanAborts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	q := New(WithStore(NewSQLiteStore(repo.DB(), dir, repo)))

	j := makeMultiFileJob(t, "ea-defer", 1, 20)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(context.Background())

	// 9 failed + 1 done = 10 resolved at a 90% failure rate, past the 80%
	// threshold. Message-IDs are read while the manifest is still attached.
	ids := make([]string, 10)
	for i := range ids {
		ids[i] = mustManifest(t, j).ArticleID(i)
	}
	ackFailed(t, q, j.ID, ids[:9]...)
	ackDone(t, q, j.ID, ids[9])

	if err := q.Pause(j.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if manifestResident(q.byID[j.ID]) {
		t.Fatal("fixture is not exercising the gate: job still resident after Pause")
	}

	// Half one: gated.
	if q.CheckEarlyAbort(j.ID) {
		t.Error("CheckEarlyAbort = true for a paused job; the verdict cannot be actioned " +
			"(no legal edge from Paused to Verifying) and the job is not downloading, " +
			"so answering true only emits a WARN for an abort that does not happen")
	}

	// The abort's consumer, to show that answering true would go nowhere.
	if _, err := q.SetPostProcStarted(j.ID); err == nil {
		t.Error("SetPostProcStarted succeeded on a paused job; the gate above is justified by " +
			"this failing, so if the status edges gained Paused->Verifying this test is the " +
			"place that should notice")
	}

	// Half two: deferred, not discarded.
	if err := q.Resume(j.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !manifestResident(q.byID[j.ID]) {
		t.Fatal("job did not become resident on Resume; the deferral half is unreachable")
	}
	if !q.CheckEarlyAbort(j.ID) {
		t.Error("CheckEarlyAbort = false after the job resumed at a 90% failure rate; " +
			"declining while non-resident must defer the verdict, not discard it")
	}
}
