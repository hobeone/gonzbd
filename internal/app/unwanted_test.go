package app_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// unwantedNZB renders an NZB with one file per subject name, one article
// each, with article IDs prefixed by tag so two NZBs never share an MD5.
func unwantedNZB(tag string, names ...string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	for i, n := range names {
		fmt.Fprintf(&b, `<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/1)">`+"\n", n)
		b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
		fmt.Fprintf(&b, `<segment bytes="1024" number="1">%s%d@t</segment>`+"\n", tag, i)
		b.WriteString("</segments>\n</file>\n")
	}
	b.WriteString("</nzb>\n")
	return []byte(b.String())
}

// setUnwantedAction writes the action directly rather than through Set:
// these fixtures' placeholder servers would fail Set's whole-config
// validation for reasons unrelated to the action.
func setUnwantedAction(t *testing.T, cfg *config.Config, action unwanted.Action) {
	t.Helper()
	if !action.Valid() {
		t.Fatalf("invalid action %q", action)
	}
	cfg.With(func(c *config.Config) { c.Downloads.ActionOnUnwantedExtensions = action })
}

// addUnwanted parses raw and adds it through BuildIngestJob and AddJob, the
// path every ingest source converges on.
func addUnwanted(t *testing.T, a *app.Application, filename string, raw []byte) (*job.Job, error) {
	t.Helper()
	parsed, err := nzb.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	j, hdr, err := app.BuildIngestJob(a.GetConfig(), parsed, filename, types.FetchOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j, a.AddJob(t.Context(), j, hdr, raw, false)
}

func rowOf(t *testing.T, a *app.Application, id string) dispatch.Row {
	t.Helper()
	row, ok := a.Dispatcher().Row(id)
	if !ok {
		t.Fatalf("job %s is not in the queue", id)
	}
	return row
}

// TestAddJob_UnwantedPause pins the default action: an NZB naming an
// unwanted file is added paused and marked blocked.
func TestAddJob_UnwantedPause(t *testing.T) {
	t.Parallel()
	a, _ := newBackupTestApp(t)
	j, err := addUnwanted(t, a, "pause.nzb", unwantedNZB("p", "movie.mkv", "Setup.EXE"))
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause", in)
	}
	if got := rowOf(t, a, j.ID()).Header.Unwanted; got != unwanted.StateBlocked {
		t.Errorf("Header.Unwanted = %d, want blocked", got)
	}
}

// TestAddJob_UnwantedCleanOrOff pins the two ways a job is not blocked: no
// file matches, or the check is off.
func TestAddJob_UnwantedCleanOrOff(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		action unwanted.Action
		files  []string
	}{
		{"nothing matches", unwanted.ActionFail, []string{"movie.mkv", "movie.nfo"}},
		{"check is off", unwanted.ActionOff, []string{"movie.mkv", "setup.exe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newBackupTestApp(t)
			setUnwantedAction(t, a.GetConfig(), c.action)
			j, err := addUnwanted(t, a, "clean.nzb", unwantedNZB("c", c.files...))
			if err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			if in := j.Intent(); in != job.IntentRun {
				t.Errorf("Intent = %v, want IntentRun", in)
			}
			if got := rowOf(t, a, j.ID()).Header.Unwanted; got != unwanted.StateNone {
				t.Errorf("Header.Unwanted = %d, want none", got)
			}
		})
	}
}

// TestAddJob_UnwantedFail pins the fail action: the job reaches history as
// Failed with the abort message and the offending name, marked blocked,
// retryable from its NZB backup, and nothing was downloaded.
func TestAddJob_UnwantedFail(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	cfg := testConfig(downloadDir, completeDir, adminDir, nntptest.New(t).ServerConfig("unwanted", 1))
	setUnwantedAction(t, cfg, unwanted.ActionFail)
	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	_, cancel := startAppAndDrain(t, a)
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	j, err := addUnwanted(t, a, "fail.nzb", unwantedNZB("f", "movie.mkv", "payload.scr"))
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	waitForHistoryAndQueueCleanup(t, repo, a, j.ID())

	e, err := repo.Get(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	if e.Status != string(constants.StatusFailed) {
		t.Errorf("Status = %q, want Failed", e.Status)
	}
	if !strings.HasPrefix(e.FailMessage, "Aborted, unwanted extension detected") || !strings.Contains(e.FailMessage, "payload.scr") {
		t.Errorf("FailMessage = %q, want the abort message naming payload.scr", e.FailMessage)
	}
	if strings.Contains(e.FailMessage, "movie.mkv") {
		t.Errorf("FailMessage = %q names a wanted file", e.FailMessage)
	}
	if e.Unwanted != unwanted.StateBlocked {
		t.Errorf("Unwanted = %d, want blocked", e.Unwanted)
	}
	if e.NZBBackup == "" {
		t.Error("no NZB backup recorded, so the entry cannot be retried")
	}
	if e.Downloaded != 0 {
		t.Errorf("Downloaded = %d, want 0", e.Downloaded)
	}
	if _, err := os.Stat(filepath.Join(downloadDir, j.Name())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("download directory exists (stat err %v); nothing should have been written", err)
	}
}

// TestAddJob_UnwantedFailsClosed pins that a check which cannot be evaluated
// refuses the job instead of adding it unchecked: once for rules that do not
// validate, once for a job whose file list cannot be read.
func TestAddJob_UnwantedFailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("rules do not validate", func(t *testing.T) {
		t.Parallel()
		a, _ := newBackupTestApp(t)
		a.GetConfig().With(func(c *config.Config) { c.Downloads.UnwantedExtensionsMode = "bogus" })
		j, err := addUnwanted(t, a, "closed.nzb", unwantedNZB("x", "movie.mkv"))
		if err == nil || !strings.Contains(err.Error(), "unwanted-extension check") {
			t.Fatalf("AddJob = %v, want an unwanted-extension check error", err)
		}
		if _, ok := a.Dispatcher().Job(j.ID()); ok {
			t.Error("the job was queued unchecked")
		}
	})
	t.Run("file list unreadable", func(t *testing.T) {
		t.Parallel()
		a, _ := newBackupTestApp(t)
		raw := unwantedNZB("y", "setup.exe")
		parsed, err := nzb.Parse(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		j, hdr, err := app.BuildIngestJob(a.GetConfig(), parsed, "evicted.nzb", types.FetchOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		j.Evict()
		err = a.AddJob(t.Context(), j, hdr, raw, false)
		if err == nil || !strings.Contains(err.Error(), "unwanted-extension check") {
			t.Fatalf("AddJob = %v, want an unwanted-extension check error", err)
		}
		if _, ok := a.Dispatcher().Job(j.ID()); ok {
			t.Error("the job was queued unchecked")
		}
	})
}

// TestAddJob_UnwantedApprovalSurvivesRestart pins that the user's approval
// of a paused job is persisted with it: after a restart the job comes back
// approved and running, not blocked and paused again.
func TestAddJob_UnwantedApprovalSurvivesRestart(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	cfg := testConfig(downloadDir, completeDir, adminDir, nntptest.New(t).ServerConfig("unwanted", 1))

	start := func() *app.Application {
		a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
		if err != nil {
			t.Fatalf("app.New: %v", err)
		}
		// Queue-wide pause, so the approved job is not granted and downloaded
		// out from under the test; per-job state is what is under test.
		a.Dispatcher().Pause()
		if err := a.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		return a
	}

	first := start()
	j, err := addUnwanted(t, first, "restart.nzb", unwantedNZB("r", "setup.exe"))
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := first.Dispatcher().ResumeJobByUser(j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	if err := first.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	second := start()
	t.Cleanup(func() {
		if err := second.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	row := rowOf(t, second, j.ID())
	if row.Header.Unwanted != unwanted.StateApproved {
		t.Errorf("restored Header.Unwanted = %d, want approved", row.Header.Unwanted)
	}
	restored, _ := second.Dispatcher().Job(j.ID())
	if in := restored.Intent(); in != job.IntentRun {
		t.Errorf("restored Intent = %v, want IntentRun", in)
	}
}

// blockedEntry files a Failed history entry for an NZB naming setup.exe,
// with the given unwanted state, and returns its ID.
func blockedEntry(t *testing.T, repo *history.Repository, adminDir, id string, state unwanted.State) {
	t.Helper()
	writeGzNZB(t, adminDir, id+".nzb.gz", unwantedNZB(id, "movie.mkv", "setup.exe"))
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:       id,
		Name:        id,
		NzbName:     id + ".nzb",
		NZBBackup:   id + ".nzb.gz",
		Status:      string(constants.StatusFailed),
		FailMessage: "Aborted, unwanted extension detected: setup.exe",
		Unwanted:    state,
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
}

// TestRetryHistoryJob_UnwantedPlainRetryIsRefused pins that a plain retry of
// an entry the check refused fails again with the same message, and changes
// nothing: the entry stays, and nothing is queued.
func TestRetryHistoryJob_UnwantedPlainRetryIsRefused(t *testing.T) {
	t.Parallel()
	a, repo, adminDir := newRetryTestApp(t)
	setUnwantedAction(t, a.GetConfig(), unwanted.ActionFail)
	const id = "unwantedretry001"
	blockedEntry(t, repo, adminDir, id, unwanted.StateBlocked)

	err := a.RetryHistoryJob(t.Context(), id)
	if !errors.Is(err, app.ErrUnwantedRefused) {
		t.Fatalf("RetryHistoryJob = %v, want ErrUnwantedRefused", err)
	}
	if !strings.Contains(err.Error(), "Aborted, unwanted extension detected: setup.exe") {
		t.Errorf("error %q does not carry the abort message", err)
	}
	if _, ok := a.Dispatcher().Job(id); ok {
		t.Error("a refused retry queued the job")
	}
	if _, err := repo.Get(t.Context(), id); err != nil {
		t.Errorf("a refused retry lost the history entry: %v", err)
	}
}

// TestRetryHistoryJob_UnwantedAllowed pins "Retry anyway": the job is queued
// approved and not paused.
func TestRetryHistoryJob_UnwantedAllowed(t *testing.T) {
	t.Parallel()
	a, repo, adminDir := newRetryTestApp(t)
	setUnwantedAction(t, a.GetConfig(), unwanted.ActionFail)
	const id = "unwantedretry002"
	blockedEntry(t, repo, adminDir, id, unwanted.StateBlocked)

	if err := a.RetryHistoryJobAllowingUnwanted(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJobAllowingUnwanted: %v", err)
	}
	row := rowOf(t, a, id)
	if row.Header.Unwanted != unwanted.StateApproved {
		t.Errorf("Header.Unwanted = %d, want approved", row.Header.Unwanted)
	}
	j, _ := a.Dispatcher().Job(id)
	if in := j.Intent(); in != job.IntentRun {
		t.Errorf("Intent = %v, want IntentRun", in)
	}
}

// TestRetryHistoryJob_UnwantedApprovalIsKept pins that an entry already
// approved keeps its approval through a plain retry.
func TestRetryHistoryJob_UnwantedApprovalIsKept(t *testing.T) {
	t.Parallel()
	a, repo, adminDir := newRetryTestApp(t)
	setUnwantedAction(t, a.GetConfig(), unwanted.ActionFail)
	const id = "unwantedretry003"
	blockedEntry(t, repo, adminDir, id, unwanted.StateApproved)

	if err := a.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}
	if got := rowOf(t, a, id).Header.Unwanted; got != unwanted.StateApproved {
		t.Errorf("Header.Unwanted = %d, want approved", got)
	}
}

// TestRetryHistoryJob_UnwantedPausesUnderPause pins that a retry honours the
// action configured now: under pause, an unapproved entry naming an
// unwanted file is queued paused and blocked rather than refused.
func TestRetryHistoryJob_UnwantedPausesUnderPause(t *testing.T) {
	t.Parallel()
	a, repo, adminDir := newRetryTestApp(t)
	const id = "unwantedretry004"
	blockedEntry(t, repo, adminDir, id, unwanted.StateBlocked)

	if err := a.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}
	if got := rowOf(t, a, id).Header.Unwanted; got != unwanted.StateBlocked {
		t.Errorf("Header.Unwanted = %d, want blocked", got)
	}
	j, _ := a.Dispatcher().Job(id)
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause", in)
	}
}
