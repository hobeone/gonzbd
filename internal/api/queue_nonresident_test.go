package api

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// newEvictedJob builds a job with two data files and one par2 recovery
// volume, adds it to a store-backed queue, and pauses it so the queue evicts
// the manifest. A store is required: without one the queue keeps every
// manifest resident and there is nothing to test.
func newEvictedJob(t *testing.T) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "movie.part01.rar", Bytes: 100, Articles: []nzb.Article{{ID: "a0@t", Bytes: 100, Number: 1}}},
			{Subject: "movie.part02.rar", Bytes: 100, Articles: []nzb.Article{{ID: "a1@t", Bytes: 100, Number: 1}}},
			{Subject: "movie.vol01+02.par2", Bytes: 50, Articles: []nzb.Article{{ID: "a2@t", Bytes: 50, Number: 1}}},
		},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "movie.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}

	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	q := queue.New(queue.WithStore(queue.NewSQLiteStore(repo.DB(), dir, repo)), queue.WithStateDir(dir))

	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Absence is the property under test here, so this asserts on the error
	// rather than going through mustManifest, which would fatal on it.
	if _, err := job.Manifest(); !errors.Is(err, queue.ErrJobNotResident) {
		t.Fatalf("fixture guard: want ErrJobNotResident after Pause, got %v — nothing is being tested", err)
	}
	return job
}

// buildSlot renders one entry of the queue listing, and the listing is
// polled continuously by the UI and by Sonarr/Radarr. It must therefore work
// for a job whose manifest has been evicted — which is every queued and
// paused job once the active set is full.
//
// It guarded m for TotalBytes and then dereferenced it unguarded two fields
// later for Par2Bytes/Par2Files, so a single non-resident job in the queue
// took the whole listing down with a 500 rather than showing a wrong number
// in one column.
func TestBuildSlot_NonResidentJob(t *testing.T) {
	t.Parallel()
	job := newEvictedJob(t)

	// Captured before the assertions so a fixture that silently stopped
	// carrying par2 would fail loudly here rather than making the checks
	// below vacuous.
	wantBytes, wantPar2Bytes, wantPar2Files := job.TotalBytes(), job.RecoveryBytes(), job.RecoveryFiles()
	if wantBytes == 0 || wantPar2Bytes == 0 || wantPar2Files == 0 {
		t.Fatalf("fixture guard: scalars unset after eviction (bytes=%d par2Bytes=%d par2Files=%d)",
			wantBytes, wantPar2Bytes, wantPar2Files)
	}

	slot := buildSlot(job, false, 0, 0, nil)

	if slot.Bytes != wantBytes {
		t.Errorf("Bytes = %d, want %d", slot.Bytes, wantBytes)
	}
	if slot.RecoveryBytes != wantPar2Bytes {
		t.Errorf("Par2Bytes = %d, want %d — a zero here renders as \"no par2 files\" for every queued job", slot.RecoveryBytes, wantPar2Bytes)
	}
	if slot.RecoveryFiles != wantPar2Files {
		t.Errorf("Par2Files = %d, want %d", slot.RecoveryFiles, wantPar2Files)
	}
}

// CurrentFile names the file being downloaded, which requires per-file
// subjects from the manifest. A non-resident job is by definition not
// downloading, so the field is empty rather than costing a disk read on
// every poll.
func TestBuildSlot_NonResidentJobHasNoCurrentFile(t *testing.T) {
	t.Parallel()
	job := newEvictedJob(t)

	if got := buildSlot(job, false, 0, 0, nil).CurrentFile; got != "" {
		t.Errorf("CurrentFile = %q, want empty for a non-resident job", got)
	}
}

// newOnDemandPar2Job builds a resident job with one content file and one
// par2 recovery volume, added with OnDemandPar2 so the recovery volume
// starts Deferred (see Job.NewJob's Deferred: isRecovery &&
// opts.OnDemandPar2). No store is needed: unlike newEvictedJob, this test
// needs the manifest resident, not evicted. Returns the queue alongside the
// job so a caller can drive ackDone through it.
func newOnDemandPar2Job(t *testing.T) (*queue.Queue, *queue.Job) {
	t.Helper()
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content.rar", Bytes: 10_000, Articles: []nzb.Article{{ID: "c1@t", Bytes: 10_000, Number: 1}}},
			// Recovery-volume subject: matches isRecoveryVolume's
			// \.vol\d+[-+]\d+\.par2 pattern, so NewJob marks it deferred.
			{Subject: "content.vol000+01.par2", Bytes: 1_000, Articles: []nzb.Article{{ID: "v1@t", Bytes: 1_000, Number: 1}}},
		},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "par2.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !job.HasDeferredPar2() {
		t.Fatal("fixture guard: recovery volume not deferred — OnDemandPar2 wiring didn't take, nothing is being tested")
	}
	return q, job
}

// TestBuildSlot_FreshOnDemandPar2JobReportsZeroPercent pins the
// deferred-par2 progress bug directly at the API layer, above ExpectedBytes's
// own unit tests: a queue row for a freshly added on-demand-par2 job must
// show 0% and Size == SizeLeft, not a false head start from the deferred
// recovery volume's bytes. TestBuildSlot_NonResidentJob above cannot catch a
// regression here — its fixture has no deferred files, so Job.TotalBytes()
// and p.ExpectedBytes() agree for it either way.
func TestBuildSlot_FreshOnDemandPar2JobReportsZeroPercent(t *testing.T) {
	t.Parallel()
	_, job := newOnDemandPar2Job(t)

	// Guard the fixture against the bug this test exists to catch: under
	// the old pairing (Size = job.TotalBytes(), the whole-manifest total
	// of 11_000) a fresh job would already show >0% downloaded even though
	// nothing has been fetched.
	if got, want := job.TotalBytes(), int64(11_000); got != want {
		t.Fatalf("fixture guard: TotalBytes() = %d, want %d", got, want)
	}

	slot := buildSlot(job, false, 0, 0, nil)

	if got, want := slot.Bytes, int64(10_000); got != want {
		t.Errorf("Bytes = %d, want %d (deferred recovery volume must not count)", got, want)
	}
	if slot.Bytes != slot.RemainingBytes {
		t.Errorf("Bytes (%d) != RemainingBytes (%d); a fresh job must report all of its expectation as still remaining",
			slot.Bytes, slot.RemainingBytes)
	}
	if got, want := slot.Percentage, 0; got != want {
		t.Errorf("Percentage = %d, want %d", got, want)
	}
}

// TestBuildSlot_DeferredPar2VolumeExcludedAfterDownload complements the
// fresh-job case above: once the content file is fully downloaded but the
// recovery volume is still deferred, the slot must report 100%, not a
// number depressed by counting the undispatched deferred bytes against it.
func TestBuildSlot_DeferredPar2VolumeExcludedAfterDownload(t *testing.T) {
	t.Parallel()
	q, job := newOnDemandPar2Job(t)

	ackDone(t, q, job.ID, "c1@t")

	slot := buildSlot(job, false, 0, 0, nil)

	if got, want := slot.Percentage, 100; got != want {
		t.Errorf("Percentage = %d, want %d (content complete, deferred volume must not count)", got, want)
	}
	if got, want := slot.RemainingBytes, int64(0); got != want {
		t.Errorf("RemainingBytes = %d, want %d", got, want)
	}
}
