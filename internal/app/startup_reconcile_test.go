package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestStart_RetentionDoesNotDefeatCrashReconciliation pins that enabling
// history retention leaves the startup duplicate-removal working.
//
// Start reconciles a crash between the history commit and Dispatcher.Remove: a
// completed job still sitting in the queue is looked up in history, and if
// the entry is there the stale queue entry is dropped. That lookup is the
// only evidence the job already finished. A retention sweep that runs first
// can delete the entry out from under it, at which point Get returns
// ErrNotFound and the job falls through to maybeFinalize — post-processed and
// re-filed a second time.
//
// The trigger is ordinary: the crash left an entry behind, the daemon stayed
// down past the retention threshold, and the operator restarted it.
func TestStart_RetentionDoesNotDefeatCrashReconciliation(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)
	// Short enough that the 30-day-old entry below is expired.
	cfg.General.HistoryRetentionDays = 1

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A completed job still in the queue — the state a crash between the
	// history commit and Dispatcher.Remove leaves behind.
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "rec1@t", Bytes: 1024, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "reconcile.nzb", types.FetchOptions{JobID: "reconcilestale01", NzbName: "reconcile"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// IsComplete keys on the per-file Complete flag, which the assembler
	// sets, not on article state.
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if !j.IsComplete() {
		t.Fatal("setup did not produce a completed job in the queue")
	}

	// Its history entry, old enough that retention wants it gone.
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     j.ID(),
		Name:      "reconcile",
		Status:    string(constants.StatusCompleted),
		Completed: time.Now().AddDate(0, 0, -30),
	}, nil); err != nil {
		t.Fatalf("seed history entry: %v", err)
	}

	application.PauseDownloads()
	application.Dispatcher().Pause()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	if _, ok := application.Dispatcher().Row(j.ID()); ok {
		t.Error("the completed job is still queued: retention deleted its history entry " +
			"before reconciliation could look it up, so it will be post-processed again")
	}
}

// TestStartupHandOffs_ChoosesCompleteJobsWithNoVerdict reads the choice
// directly, over one registry holding every shape: never run, at Fetching
// with and without a recorded verdict, past Fetching, and incomplete. It also
// covers an application with no dispatcher.
func TestStartupHandOffs_ChoosesCompleteJobsWithNoVerdict(t *testing.T) {
	t.Parallel()
	if got := (&Application{}).startupHandOffs(); len(got) != 0 {
		t.Errorf("no dispatcher: got %v, want none", got)
	}

	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)
	application, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := application.Dispatcher()
	add := func(id string, complete bool, moves ...func(*job.Job)) {
		t.Helper()
		parsed := &nzb.NZB{Files: []nzb.File{{
			Subject: id + ".bin", Bytes: 1024,
			Articles: []nzb.Article{{ID: id + "@t", Bytes: 1024, Number: 1}},
		}}}
		j, hdr, err := BuildIngestJob(application.config, parsed, id+".nzb",
			types.FetchOptions{JobID: id, NzbName: id}, nil)
		if err != nil {
			t.Fatalf("BuildIngestJob: %v", err)
		}
		if err := d.Add(t.Context(), j, hdr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if complete {
			if err := j.MarkFileComplete(0); err != nil {
				t.Fatalf("MarkFileComplete: %v", err)
			}
		}
		for _, m := range moves {
			m(j)
		}
	}
	begin := func(j *job.Job) {
		if err := j.BeginAttempt(time.Now()); err != nil {
			t.Fatalf("BeginAttempt: %v", err)
		}
	}
	verdict := func(j *job.Job) {
		if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
			t.Fatalf("AdvanceFrom: %v", err)
		}
	}
	assessing := func(j *job.Job) {
		if err := j.Transition(job.Assessing); err != nil {
			t.Fatalf("Transition: %v", err)
		}
	}

	add("neverrun00000001", true)
	add("fetching00000001", true, begin)
	add("verdict000000001", true, begin, verdict)
	add("assessing0000001", true, begin, verdict, assessing)
	add("incomplete000001", false, begin)

	got := application.startupHandOffs()
	want := map[string]bool{"neverrun00000001": true, "fetching00000001": true}
	if len(got) != len(want) {
		t.Errorf("startupHandOffs = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("startupHandOffs = %v, want it to hold %s", got, id)
		}
	}
}

// TestStart_HandsOffOnlyCompleteJobsWithNoVerdict pins which restored jobs
// Start hands to post-processing itself.
//
// A job whose download-complete report is recorded (Fetching{next: Assessing})
// belongs to the dispatcher: its first unpaused tick moves it to Assessing and
// launches runAssess, which hands it over. A hand-off from Start as well runs
// post-processing beside that worker. The dispatcher is paused here so no tick
// moves any job, which leaves Start's own hand-off as the only way into
// post-processing.
//
// The stage holds every job it is given, so the admission of a job Start
// handed over is still in place when Start returns.
func TestStart_HandsOffOnlyCompleteJobsWithNoVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		begin    bool // BeginAttempt: Fetching rather than never run
		complete bool
		verdict  bool // the download-complete report is recorded
		want     bool
	}{
		{name: "never run, complete", complete: true, want: true},
		{name: "fetching, complete, no verdict", begin: true, complete: true, want: true},
		{name: "fetching, complete, verdict recorded", begin: true, complete: true, verdict: true, want: false},
		{name: "fetching, incomplete", begin: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adminDir := t.TempDir()
			cfg := testConfigInternal(t, adminDir)
			db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
			if err != nil {
				t.Fatalf("history.Open: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			repo := history.NewRepository(db)

			stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
			application, err := New(cfg, repo, WithPostProcStages([]postproc.Stage{stage}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			parsed := &nzb.NZB{Files: []nzb.File{
				{Subject: "a.bin", Bytes: 1024, Articles: []nzb.Article{{ID: "a1@t", Bytes: 1024, Number: 1}}},
				{Subject: "b.bin", Bytes: 1024, Articles: []nzb.Article{{ID: "b1@t", Bytes: 1024, Number: 1}}},
			}}
			j, hdr, err := BuildIngestJob(application.config, parsed, "handoff.nzb",
				types.FetchOptions{JobID: "startuphandoff01", NzbName: "handoff"}, nil)
			if err != nil {
				t.Fatalf("BuildIngestJob: %v", err)
			}
			d := application.Dispatcher()
			if err := d.Add(t.Context(), j, hdr); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if tc.begin {
				if err := j.BeginAttempt(time.Now()); err != nil {
					t.Fatalf("BeginAttempt: %v", err)
				}
			}
			if err := j.MarkFileComplete(0); err != nil {
				t.Fatalf("MarkFileComplete: %v", err)
			}
			if tc.complete {
				if err := j.MarkFileComplete(1); err != nil {
					t.Fatalf("MarkFileComplete: %v", err)
				}
			}
			if tc.verdict {
				if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
					t.Fatalf("AdvanceFrom: %v", err)
				}
			}
			if j.IsComplete() != tc.complete {
				t.Fatalf("setup: IsComplete = %v, want %v", j.IsComplete(), tc.complete)
			}
			// An empty download directory skips every stage, and the admission
			// would end before it could be read.
			dir := filepath.Join(cfg.General.DownloadDir, j.Name())
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 1024), 0o600); err != nil {
				t.Fatal(err)
			}

			application.PauseDownloads()
			d.Pause()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := application.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = application.Shutdown() })
			t.Cleanup(func() { close(stage.finish) })

			if got := application.postProcAdmissions.has(j); got != tc.want {
				t.Errorf("handed to post-processing by Start = %v, want %v (state %+v)",
					got, tc.want, j.Snapshot().State)
			}
		})
	}
}
