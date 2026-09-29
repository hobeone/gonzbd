package app_test

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// heldStage never finishes a job on its own: it returns only when its context
// is cancelled. It keeps a job that reaches post-processing from moving its
// files out of the download directory while the test still reads them.
type heldStage struct{}

func (heldStage) Name() string { return "held" }

func (heldStage) Run(ctx context.Context, _ *postproc.Job) error {
	<-ctx.Done()
	return ctx.Err()
}

// resumeSweepTickWindow is how long the wrapped resumer gives a running
// dispatcher to move the job while the sweep is looking at it. The dispatcher
// ticks at once on the wake its restore primes, and again every second, so a
// dispatcher that is already running moves the job well inside this window.
const resumeSweepTickWindow = 1500 * time.Millisecond

// TestResumeAtStartup_RestoredVerdictDoesNotOutrunTheSweep pins the ordering
// between the startup resume sweep and the dispatcher's first tick, for the
// one restored shape where the tick can take a job out of the sweep's reach.
//
// A queue row can be restored at Fetching{next: Assessing}: a crash between
// the queue save that recorded the verdict and the job_files flush that
// would have recorded the last file's Complete flag leaves exactly that. The
// sweep repairs such a file only for a job at Fetching. A tick that runs
// first moves the job to Assessing, the sweep skips it, and the file is never
// trimmed or marked complete.
//
// Two assertions, because the race has two losing interleavings. A tick
// before the sweep reads the registry makes the sweep skip the job, which the
// file assertions catch. A tick while the sweep is on the job does not stop
// the repair, so only the wrapped resumer sees it — and it is what makes this
// test fail every time rather than whenever the tick happens to win.
func TestResumeAtStartup_RestoredVerdictDoesNotOutrunTheSweep(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writePartial(0, 1, 2)
	// Pre-allocation's untrimmed tail: the finalize the crash interrupted is
	// the one that would have cut it.
	tail := make([]byte, resumePartLen)
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := fh.Write(tail); err != nil {
		t.Fatalf("append tail: %v", err)
	}
	if err := fh.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.recordRuns(0, 1, 2)

	row := f.row
	row.State.Next = job.Assessing
	if err := dispatchstore.New(f.repo.DB()).Save(t.Context(), row); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	var movedDuringSweep atomic.Bool
	a := f.startWith(1, []postproc.Stage{heldStage{}}, func(a *app.Application) {
		a.WrapResumer(func(next app.ResumeFunc) app.ResumeFunc {
			return func(ctx context.Context, jobID string, fileIdx int32, path string) (durability.ResumeResult, error) {
				if jobID == f.jobID {
					moved := waitUntil(resumeSweepTickWindow, func() bool {
						j, ok := a.Dispatcher().Job(jobID)
						return !ok || j.Snapshot().State.State != job.Fetching
					})
					if moved {
						movedDuringSweep.Store(true)
					}
				}
				return next(ctx, jobID, fileIdx, path)
			}
		})
	})

	if movedDuringSweep.Load() {
		t.Error("the dispatcher moved the restored job out of Fetching while the resume sweep " +
			"was on it; the sweep repairs only jobs at Fetching, so a tick that wins this race " +
			"leaves the stranded file untrimmed and incomplete")
	}
	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatalf("stat after Start: %v", err)
	}
	if fi.Size() != resumeTotal {
		t.Errorf("file is %d bytes after Start, want %d — the sweep did not finish the "+
			"interrupted finalize, so pre-allocation's tail reaches post-processing",
			fi.Size(), resumeTotal)
	}
	j, ok := a.Dispatcher().Job(f.jobID)
	if !ok {
		t.Fatal("job left the dispatcher before it could be inspected")
	}
	if !j.Progress().FileComplete(0) {
		t.Error("the file is not Complete after Start; the sweep skipped a job the first " +
			"tick had already moved to Assessing")
	}
}
