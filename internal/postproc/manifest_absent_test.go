package postproc

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/queue"
)

// evictedJob returns a postproc Job whose queue job has no resident
// manifest, the state a job reaches when its manifest file cannot be read.
func evictedJob(t *testing.T) *Job {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.part01.rar", Bytes: 100, Articles: []nzb.Article{{ID: "a0@t", Bytes: 100, Number: 1}}},
	}}
	qjob, err := queue.NewJob(parsed, queue.AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	// A store is required for the queue to evict on pause; without one every
	// manifest stays resident and there is nothing to test.
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	q := queue.New(queue.WithStore(queue.NewSQLiteStore(repo.DB(), dir, repo)), queue.WithStateDir(dir))
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Pause(qjob.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, mErr := qjob.Manifest(); mErr == nil {
		t.Fatal("fixture guard: manifest still resident, nothing is being tested")
	}
	return &Job{Queue: qjob}
}

// The file listing is reporting, so it degrades — but it says why, in the
// record itself. An empty listing and a job that downloaded nothing look
// identical to whoever reads the history entry later.
func TestBuildDownloadFileList_AbsentManifestExplainsItself(t *testing.T) {
	lines := buildDownloadFileList(evictedJob(t))

	if len(lines) == 0 {
		t.Fatal("returned no lines at all; the history record would show an empty download stage with no explanation")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "File listing unavailable") {
		t.Errorf("listing does not explain its own absence:\n%s", joined)
	}
}

// Quickcheck must not report a job as CRC-verified when it verified
// nothing. QuickCheckRan is what downstream reads, so the failure has to
// stop before that flag is set.
func TestVerifyJobCRCs_AbsentManifestErrorsRatherThanClaimingVerified(t *testing.T) {
	job := evictedJob(t)
	q := &QuickCheckStage{}

	err := q.verifyJobCRCs(context.Background(), slog.Default(), job, []par2.Set{})

	if err == nil {
		t.Fatal("verifyJobCRCs returned nil with no manifest; the stage log would record a clean pass over nothing")
	}
	if job.QuickCheckRan {
		t.Error("QuickCheckRan was set despite no verification happening — downstream cannot tell this apart from a real check")
	}
	if job.QuickCheckPassed {
		t.Error("QuickCheckPassed was set despite no verification happening")
	}
}
