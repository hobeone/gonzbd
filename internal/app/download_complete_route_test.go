package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// verdictWait bounds the wait for runAssess's on-demand par2 verdict. The
// dispatcher ticks at once on the wake its restore or Add primes, and every
// second after that; a job takes two ticks to reach Assessing.
const verdictWait = 10 * time.Second

// unassessedStage reports each job post-processing starts before runAssess
// has released its recovery volume, then holds the job until its context
// ends. A job can reach post-processing legitimately after the release: with
// no server to fetch it from, the volume's article fails, which completes the
// file and sends the job through Assessing again.
type unassessedStage struct {
	app        *atomic.Pointer[app.Application]
	unassessed chan string
}

func (unassessedStage) Name() string { return "unassessed" }

func (s unassessedStage) Run(ctx context.Context, pj *postproc.Job) error {
	recovered := false
	if a := s.app.Load(); a != nil {
		if j, ok := a.Dispatcher().Job(pj.JobID()); ok {
			p := j.Progress()
			recovered = p != nil && p.Par2Recovered()
		}
	}
	if !recovered {
		select {
		case s.unassessed <- pj.JobID():
		default:
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// deferredVolumeNZB is one payload file and one par2 recovery volume, which
// BuildIngestJob defers while on-demand par2 is on.
func deferredVolumeNZB(prefix string) *nzb.NZB {
	return &nzb.NZB{Files: []nzb.File{
		{Subject: `"payload.bin" yEnc (1/1)`, Bytes: 1024,
			Articles: []nzb.Article{{ID: prefix + "p1@t", Bytes: 1024, Number: 1}}},
		{Subject: `"payload.vol000+01.par2" yEnc (1/1)`, Bytes: 1024,
			Articles: []nzb.Article{{ID: prefix + "r1@t", Bytes: 1024, Number: 1}}},
	}}
}

// persistCompleteJob writes what a previous process leaves for a queued job
// built from deferredVolumeNZB: its manifest, its queue row at state, and
// job_files rows recording the payload file complete and the recovery volume
// deferred.
func persistCompleteJob(t *testing.T, repo *history.Repository, adminDir string,
	j *job.Job, hdr dispatch.Header, state job.StateView, sortKey int64,
) {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	vol := recoveryFileIndex(t, m)
	manifestDir := filepath.Join(adminDir, "queue", "manifests")
	if err := os.MkdirAll(manifestDir, 0o750); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(manifestDir, j.ID()+".json.gz"), data); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := dispatchstore.New(repo.DB()).Save(t.Context(), dispatch.Persisted{
		ID: j.ID(), SortKey: sortKey, Header: hdr, Policy: j.Policy(),
		State: state, Intent: j.Intent(),
	}); err != nil {
		t.Fatalf("store.Save: %v", err)
	}
	for fi := range m.NumFiles() {
		complete, fetch := 1, job.FetchAlways
		if fi == vol {
			complete, fetch = 0, job.FetchIfNeeded
		}
		if _, err := repo.DB().ExecContext(t.Context(),
			`INSERT INTO job_files (job_id, file_index, complete, assembled_crc32, fetch_policy, filename)
			VALUES (?, ?, ?, 0, ?, '')`,
			j.ID(), fi, complete, int(fetch)); err != nil {
			t.Fatalf("insert job_files: %v", err)
		}
	}
}

// writePayload puts the payload file in the job's download directory.
// Post-processing skips every stage of a job whose directory is empty, so
// without it a job handed straight to post-processing would never reach
// unassessedStage.
func writePayload(t *testing.T, downloadDir, jobName string) {
	t.Helper()
	dir := filepath.Join(downloadDir, jobName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startRouteApp starts an application over repo with on-demand par2 on, no
// enabled server, and unassessedStage as its only post-processing stage.
func startRouteApp(t *testing.T, repo *history.Repository, adminDir, downloadDir, completeDir string, beforeStart ...func(*app.Application)) (*app.Application, unassessedStage) {
	t.Helper()
	cfg := testConfig(downloadDir, completeDir, adminDir, config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	cfg.With(func(c *config.Config) { c.Downloads.OnDemandPar2 = true })
	stage := unassessedStage{app: new(atomic.Pointer[app.Application]), unassessed: make(chan string, 4)}
	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{stage}))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	stage.app.Store(a)
	for _, f := range beforeStart {
		f(a)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		a.ForceStopWorkers()
		cancel()
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())
	return a, stage
}

// assertAssessed waits for runAssess to rule on the job's deferred recovery
// volume, and checks that post-processing did not start the job before that.
//
// The directory holds no par2 index, so maybeReleaseRecoveryVolumes rules
// repair and releases the volume, which sets Par2Recovered. Par2Recovered is
// the signal rather than HasDeferredPar2, because a job with no progress
// record reports no deferred volume either.
func assertAssessed(t *testing.T, a *app.Application, stage unassessedStage, id string) {
	t.Helper()
	assessed := waitUntil(verdictWait, func() bool {
		j, ok := a.Dispatcher().Job(id)
		if !ok {
			return false
		}
		p := j.Progress()
		return p != nil && p.Par2Recovered()
	})
	if !assessed {
		t.Errorf("the complete job never reached the on-demand par2 verdict: " +
			"its deferred recovery volume was not released, so a repair runs without it")
	} else {
		// The verdict is reported, not replaced by a failure: a healthy job
		// leaves Assessing for Fetching to fetch the volume it released.
		var row dispatch.Row
		var ok bool
		waitUntil(verdictWait, func() bool {
			row, ok = a.Dispatcher().Row(id)
			return !ok || row.View.Outcome.IsSettled() || row.View.State != job.Assessing || row.View.Next != job.StateUnset
		})
		switch {
		case !ok:
			t.Errorf("job %s left the queue after its par2 verdict, want it fetching the recovery volume the verdict released", id)
		case row.View.Outcome == job.OutcomeFailed:
			t.Errorf("job %s was settled Failed at %v after its par2 verdict, want the verdict reported", id, row.View.State)
		}
	}
	select {
	case got := <-stage.unassessed:
		t.Errorf("job %s reached post-processing before the on-demand par2 verdict", got)
	default:
	}
}

// TestRestart_CompleteJobPassesThroughAssessing pins how a complete job that
// a previous process left in the queue reaches post-processing: through
// Assessing, whose runAssess gives the on-demand par2 verdict, as a job
// whose last file completes in this process does.
//
// The rows are the shapes a crash leaves. Never run: a retry of a job whose
// files were all complete, before its first tick. Fetching with no Next: a
// checkpoint flush recorded the last file's Complete flag, and the queue save
// of the download-complete report did not happen. Fetching with Next
// recorded is the shape whose report was saved.
func TestRestart_CompleteJobPassesThroughAssessing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		begin   bool // an attempt was opened: Fetching rather than never run
		verdict bool // the download-complete report was saved
	}{
		{name: "never run"},
		{name: "fetching, no verdict", begin: true},
		{name: "fetching, verdict recorded", begin: true, verdict: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
			cfg := testConfig(downloadDir, completeDir, adminDir)
			cfg.With(func(c *config.Config) { c.Downloads.OnDemandPar2 = true })
			// PPRepair: assertAssessed waits for the verdict to release the
			// recovery volume, which is gated on Policy.Repair. A PP=0 job
			// (the zero FetchOptions.PP) never releases it, by design.
			j, hdr := buildTestJob(t, cfg, deferredVolumeNZB("route"), types.FetchOptions{NzbName: "restart-route", PP: types.PPRepair})
			if !j.HasDeferredPar2() {
				t.Fatal("setup: the recovery volume is not deferred")
			}
			if tc.begin {
				if err := j.BeginAttempt(time.Now()); err != nil {
					t.Fatalf("BeginAttempt: %v", err)
				}
			}
			state := j.Checkpoint().State
			if tc.verdict {
				state.Next = job.Assessing
			}
			persistCompleteJob(t, repo, adminDir, j, hdr, state, int64(i+1))

			writePayload(t, downloadDir, j.Name())
			a, stage := startRouteApp(t, repo, adminDir, downloadDir, completeDir)
			assertAssessed(t, a, stage, j.ID())
		})
	}
}

// TestRetryHistoryJob_CompleteJobPassesThroughAssessing pins that a retry of
// a failed job whose files are all on disk reaches post-processing through
// Assessing. The retry re-derives the fetch policy, so its recovery volume is
// deferred again and needs the on-demand par2 verdict before any repair.
func TestRetryHistoryJob_CompleteJobPassesThroughAssessing(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	a, stage := startRouteApp(t, repo, adminDir, downloadDir, completeDir)

	writeGzNZB(t, adminDir, "retryroute.nzb.gz", retryNZBWithRecoveryVolume(2, 1))
	const id = "retryrouteassess"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "retryroute",
		NzbName:   "retryroute.nzb",
		NZBBackup: "retryroute.nzb.gz",
		Status:    string(constants.StatusFailed),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	seedCompletedFile(t, repo.DB(), id, 0, 0, 2)
	seedHistoryJobFilesRow(t, repo.DB(), id, 1, false, 1, job.FetchIfNeeded)
	writePayload(t, downloadDir, "retryroute")

	if err := a.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}
	assertAssessed(t, a, stage, id)
}

// TestRestart_DropsAJobAlreadyInHistoryBeforeTheResumeSweep pins where
// startup drops a queued job that is already filed in history: before the
// resume sweep, and so before the dispatcher's first tick. A tick routes a
// complete job onward, so a duplicate still queued then could be
// post-processed a second time.
//
// The resume fixture's job is the witness: the sweep stats its named file,
// so the wrapped resumer runs inside the sweep and looks for the duplicate.
func TestRestart_DropsAJobAlreadyInHistoryBeforeTheResumeSweep(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	cfg := testConfig(f.downloadDir, f.completeDir, f.adminDir)
	dup, hdr := buildTestJob(t, cfg, deferredVolumeNZB("dup"), types.FetchOptions{NzbName: "duplicate"})
	if err := dup.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	persistCompleteJob(t, f.repo, f.adminDir, dup, hdr, dup.Checkpoint().State, 2)
	if err := f.repo.Add(t.Context(), history.Entry{
		NzoID: dup.ID(), Name: "duplicate", Status: string(constants.StatusCompleted),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	var swept, queuedDuringSweep atomic.Bool
	a := f.startWith(1, []postproc.Stage{heldStage{}}, func(a *app.Application) {
		a.WrapResumer(func(next app.ResumeFunc) app.ResumeFunc {
			return func(ctx context.Context, jobID string, fileIdx int32, path string) (durability.ResumeResult, error) {
				if jobID == f.jobID {
					swept.Store(true)
					if _, ok := a.Dispatcher().Job(dup.ID()); ok {
						queuedDuringSweep.Store(true)
					}
				}
				return next(ctx, jobID, fileIdx, path)
			}
		})
	})

	if !swept.Load() {
		t.Fatal("the resume sweep never reached the witness job, so this test observed nothing")
	}
	if queuedDuringSweep.Load() {
		t.Error("the job already in history was still queued during the resume sweep; " +
			"it is dropped after the first tick, which can route it onward first")
	}
	if _, ok := a.Dispatcher().Job(dup.ID()); ok {
		t.Error("the job already in history is still queued after Start")
	}
}
