package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// failMsgFile describes one file for buildFailMsgJob: its subject and its
// size. What makes a file count toward RecoveryBytes/RecoveryFiles is a
// recovery-volume subject (".volNNN+MM.par2"), not a bare ".par2" suffix — an
// index file is classified as content, since it conventionally holds
// checksums rather than recovery slices.
// Each file gets exactly one article of that size, so failing article i fails
// precisely file i's bytes.
type failMsgFile struct {
	subject string
	bytes   int64
}

// buildFailMsgJob adds a job built from files to a fresh queue and fails the
// articles at failIdx, returning the job as failMsgForJob will see it.
func buildFailMsgJob(t *testing.T, files []failMsgFile, failIdx ...int) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{}
	for i, f := range files {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  f.subject,
			Bytes:    f.bytes,
			Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: int(f.bytes), Number: 1}},
		})
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "t.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, i := range failIdx {
		if _, err := q.MarkArticleFailed(job.ID, fmt.Sprintf("a%d@t", i)); err != nil {
			t.Fatalf("MarkArticleFailed(%d): %v", i, err)
		}
	}
	return job
}

// failMsgForJob decides whether a partially-failed job is worth handing to
// post-processing or is beyond repair, and its output is the message the
// user sees on an aborted job. Every branch is a different verdict, so each
// one is pinned here rather than covered incidentally through the pipeline.
func TestFailMsgForJob(t *testing.T) {
	const (
		dataA = "movie.part01.rar"
		dataB = "movie.part02.rar"
		par2  = "movie.vol01+02.par2"
	)

	tests := []struct {
		name     string
		files    []failMsgFile
		failIdx  []int
		wantSub  string // "" means the empty message: proceed to post-processing
		wantKeep bool   // true when the empty message is the expected verdict
	}{
		{
			name:     "no failures proceeds",
			files:    []failMsgFile{{dataA, 100}, {dataB, 100}},
			wantKeep: true,
		},
		{
			name:    "every article failed is beyond repair regardless of par2",
			files:   []failMsgFile{{dataA, 100}, {par2, 100}},
			failIdx: []int{0, 1},
			wantSub: "All articles failed",
		},
		{
			name:    "failure exceeding par2 capacity is beyond repair",
			files:   []failMsgFile{{dataA, 100}, {dataB, 100}, {par2, 50}},
			failIdx: []int{0},
			wantSub: "exceeds repair capacity",
		},
		{
			name:    "any failure with no par2 at all is beyond repair",
			files:   []failMsgFile{{dataA, 100}, {dataB, 100}},
			failIdx: []int{0},
			wantSub: "no par2 files available",
		},
		{
			name:     "failure within par2 capacity proceeds to repair",
			files:    []failMsgFile{{dataA, 100}, {dataB, 100}, {par2, 150}},
			failIdx:  []int{0},
			wantKeep: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := buildFailMsgJob(t, tc.files, tc.failIdx...)
			got := failMsgForJob(job)

			if tc.wantKeep {
				if got != "" {
					t.Errorf("failMsgForJob() = %q, want empty (job should proceed to post-processing)", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("failMsgForJob() = %q, want it to contain %q", got, tc.wantSub)
			}
		})
	}
}

// The same verdicts must hold with no resident manifest: failMsgForJob runs
// on the startup recovery walk over Queue.Snapshot(), where a job may have
// been evicted. It used to read job.Manifest().TotalBytes() and would have
// nil-dereferenced there.
func TestFailMsgForJob_WithoutResidentManifest(t *testing.T) {
	job := buildFailMsgJob(t,
		[]failMsgFile{{"movie.part01.rar", 100}, {"movie.part02.rar", 100}, {"movie.vol01+02.par2", 50}},
		0)

	want := failMsgForJob(job)
	if !strings.Contains(want, "exceeds repair capacity") {
		t.Fatalf("fixture guard: got %q while resident, want the exceeds-capacity verdict", want)
	}

	// A store is required for eviction: without one the queue keeps every
	// manifest resident, and Pause would leave nothing to test.
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
	if _, err := job.Manifest(); !errors.Is(err, queue.ErrJobNotResident) {
		t.Fatalf("fixture guard: want ErrJobNotResident after Pause, got %v — the eviction path is not being exercised", err)
	}

	if got := failMsgForJob(job); got != want {
		t.Errorf("failMsgForJob() after eviction = %q, want %q — the verdict must not depend on manifest residency", got, want)
	}
}
