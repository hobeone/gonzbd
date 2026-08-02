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
	wantBytes, wantPar2Bytes, wantPar2Files := job.TotalBytes(), job.Par2Bytes(), job.Par2Files()
	if wantBytes == 0 || wantPar2Bytes == 0 || wantPar2Files == 0 {
		t.Fatalf("fixture guard: scalars unset after eviction (bytes=%d par2Bytes=%d par2Files=%d)",
			wantBytes, wantPar2Bytes, wantPar2Files)
	}

	slot := buildSlot(job, false, 0, 0, nil)

	if slot.Bytes != wantBytes {
		t.Errorf("Bytes = %d, want %d", slot.Bytes, wantBytes)
	}
	if slot.Par2Bytes != wantPar2Bytes {
		t.Errorf("Par2Bytes = %d, want %d — a zero here renders as \"no par2 files\" for every queued job", slot.Par2Bytes, wantPar2Bytes)
	}
	if slot.Par2Files != wantPar2Files {
		t.Errorf("Par2Files = %d, want %d", slot.Par2Files, wantPar2Files)
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
