package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

type reportRecorder struct {
	mu    sync.Mutex
	total int
}

func (r *reportRecorder) Finished(_ string, _ job.Outcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	return nil
}

func (r *reportRecorder) Yielded(_ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	return nil
}

func (r *reportRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

func waitFor(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// TestAppRunner_ReturnsPromptly pins ports.go's hardest requirement: Run is
// called from the dispatcher's tick goroutine, so blocking there stalls every
// other job's advance -- not just this one's.
func TestAppRunner_ReturnsPromptly(t *testing.T) {
	r := newAppRunner(newTestApplication(t))

	done := make(chan struct{})
	go func() {
		r.Run(context.Background(), "abc123", job.Fetching)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked for 2s; it must dispatch work and return")
	}
}

// TestAppRunner_EveryStateReportsExactlyOnce pins the resource contract: a
// state that returns without calling Finished or Yielded strands the job's
// lease and compute slot forever, because the Queue cannot tell 'holding and
// working' from 'holding and yielded'.
func TestAppRunner_EveryStateReportsExactlyOnce(t *testing.T) {
	for _, st := range job.AllStates() {
		t.Run(st.String(), func(t *testing.T) {
			app := newTestApplication(t)
			rec := &reportRecorder{}
			r := newAppRunner(app)
			r.report = rec

			r.Run(context.Background(), "abc123", st)
			waitFor(t, func() bool { return rec.calls() == 1 })

			if got := rec.calls(); got != 1 {
				t.Fatalf("state %s reported %d times, want exactly 1", st, got)
			}
		})
	}
}

func TestAppRunner_DirectMethods(t *testing.T) {
	app := newTestApplication(t)
	rec := &reportRecorder{}
	r := newAppRunner(app)
	r.report = rec

	r.runFetch(context.Background(), "unknown")
	r.runAssess(context.Background(), "unknown")
	r.runPostProc(context.Background(), "unknown", job.Repairing)

	if got := rec.calls(); got != 3 {
		t.Errorf("got %d calls, want 3", got)
	}
}

func buildRunnerJob(t *testing.T, app *Application, files []failMsgFile, failIdx ...int) *job.Job {
	t.Helper()
	parsed := &nzb.NZB{}
	for i, f := range files {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  f.subject,
			Bytes:    f.bytes,
			Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: int(f.bytes), Number: 1}},
		})
	}
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
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := j.Transition(job.Assessing); err != nil {
		t.Fatalf("Transition(Assessing): %v", err)
	}
	return j
}

func TestAppRunner_RunAssessBranches(t *testing.T) {
	app := newTestApplication(t)
	rec := &reportRecorder{}
	r := newAppRunner(app)
	r.report = rec

	// Default branch in Run
	r.Run(context.Background(), "j_default", job.State(250))
	if got := rec.calls(); got != 1 {
		t.Fatalf("default state should yield once; got %d", got)
	}

	// Nil app in runAssess
	rNil := &appRunner{report: rec}
	rNil.runAssess(context.Background(), "unknown")

	// 1. Hopeless job
	jHopeless := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
	}, 0)
	r.runAssess(context.Background(), jHopeless.ID())

	// 2. Repairable job
	jRepair := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
		{subject: "movie.vol01+02.par2", bytes: 200},
	}, 0)
	r.runAssess(context.Background(), jRepair.ID())
	if jRepair.Checkpoint().State.Next != job.Repairing {
		t.Errorf("jRepair.Checkpoint().State.Next = %v, want Repairing", jRepair.Checkpoint().State.Next)
	}

	// 3. Deferred recovery volume release (maybeReleaseRecoveryVolumes == true)
	jDeferred := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
		{subject: "movie.vol01+02.par2", bytes: 200},
	})
	_ = jDeferred.SetFileFetchPolicy(1, job.FetchIfNeeded)
	r.runAssess(context.Background(), jDeferred.ID())
	if jDeferred.Checkpoint().State.Next != job.Fetching {
		t.Errorf("jDeferred.Checkpoint().State.Next = %v, want Fetching", jDeferred.Checkpoint().State.Next)
	}

	// 4. Intact job
	jClean := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
	})
	r.runAssess(context.Background(), jClean.ID())
	if jClean.Checkpoint().State.Next != job.Extracting {
		t.Errorf("jClean.Checkpoint().State.Next = %v, want Extracting", jClean.Checkpoint().State.Next)
	}
}
