package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
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

// buildFailMsgJob adds a job built from files to a fresh dispatcher and fails the
// articles at failIdx, returning the job as failMsgForJob will see it.
func buildFailMsgJob(t *testing.T, files []failMsgFile, failIdx ...int) *job.Job {
	t.Helper()
	parsed := &nzb.NZB{}
	for i, f := range files {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  f.subject,
			Bytes:    f.bytes,
			Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: int(f.bytes), Number: 1}},
		})
	}
	app := newTestApplication(t)
	j, hdr, err := BuildIngestJob(app.config, parsed, "t.nzb", types.FetchOptions{NzbName: "t.nzb"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := app.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, i := range failIdx {
		ackFailed(t, app.Dispatcher(), j.ID(), fmt.Sprintf("a%d@t", i))
	}
	return j
}

// failMsgForJob decides whether a partially-failed job is worth handing to
// post-processing or is beyond repair, and its output is the message the
// user sees on an aborted job. Every branch is a different verdict, so each
// one is pinned here rather than covered incidentally through the pipeline.
func TestFailMsgForJob(t *testing.T) {
	t.Parallel()
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
// on the startup recovery walk, where a job may have
// been evicted. It used to read job.Manifest().TotalBytes() and would have
// nil-dereferenced there.
func TestFailMsgForJob_WithoutResidentManifest(t *testing.T) {
	t.Parallel()
	testJob := buildFailMsgJob(t,
		[]failMsgFile{{"movie.part01.rar", 100}, {"movie.part02.rar", 100}, {"movie.vol01+02.par2", 50}},
		0)

	want := failMsgForJob(testJob)
	if !strings.Contains(want, "exceeds repair capacity") {
		t.Fatalf("fixture guard: got %q while resident, want the exceeds-capacity verdict", want)
	}

	testJob.Evict()
	if _, err := testJob.Manifest(); !errors.Is(err, job.ErrNotResident) {
		t.Fatalf("fixture guard: want ErrNotResident after Evict, got %v", err)
	}

	if got := failMsgForJob(testJob); got != want {
		t.Errorf("failMsgForJob() after eviction = %q, want %q — the verdict must not depend on manifest residency", got, want)
	}
}

type fakeProgressCounters struct {
	failed  int64
	content int64
	exp     int64
}

func (f fakeProgressCounters) ProgressFigures() (int64, int64, int64) { return f.exp, 0, f.failed }
func (f fakeProgressCounters) FailedBytes() int64                     { return f.failed }
func (f fakeProgressCounters) ContentFailedBytes() int64              { return f.content }
func (f fakeProgressCounters) ExpectedBytes() int64                   { return f.exp }
func (f fakeProgressCounters) RemainingBytes() int64                  { return 0 }

func TestFailMsgForCounters_Direct(t *testing.T) {
	t.Parallel()
	p := fakeProgressCounters{failed: 100, content: 100, exp: 100}
	msg := failMsgForCounters(p, "", 0, 0, false)
	if !strings.Contains(msg, "Job is beyond repair") {
		t.Errorf("failMsgForCounters = %q, want 'Job is beyond repair'", msg)
	}
}

func TestFailMsgForJob_NilJob(t *testing.T) {
	t.Parallel()
	if got := failMsgForJob(nil); got != "" {
		t.Errorf("failMsgForJob(nil) = %q, want empty string", got)
	}
}
