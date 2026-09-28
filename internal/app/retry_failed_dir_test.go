package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// failedDirRetry is a two-file job run through the real finalize stage with
// folder_rename on. File "kept.bin" downloads in full on the first attempt;
// the second article of "partial.bin" is missing from the server, so that
// attempt fails and finalize renames the job directory to _FAILED_<name>.
type failedDirRetry struct {
	t           *testing.T
	cfg         *config.Config
	a           *app.Application
	stop        func()
	repo        *history.Repository
	srv         *nntptest.Scripted
	id          string
	downloadDir string
	completeDir string
	name        string
	kept        [2][]byte
	partial     [2][]byte
	keptIDs     [2]string
	partialIDs  [2]string
	// fail is the stand-in repair verdict: while set, the job fails
	// post-processing the way an unrepairable download does.
	fail atomic.Bool
}

// failVerdictStage fails the job while fail is set. It runs ahead of the
// real finalize stage, which is what reads the verdict.
type failVerdictStage struct{ fail *atomic.Bool }

func (failVerdictStage) Name() string { return "fail-verdict" }

func (s failVerdictStage) Run(_ context.Context, j *postproc.Job) error {
	if s.fail.Load() {
		j.ParError = true
	}
	return nil
}

func newFailedDirRetry(t *testing.T) *failedDirRetry {
	t.Helper()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	srv := nntptest.New(t)
	cfg := testConfig(downloadDir, completeDir, adminDir, srv.ServerConfig("failed-dir", 2))
	s := &failedDirRetry{
		t: t, cfg: cfg, repo: repo, srv: srv,
		downloadDir: downloadDir, completeDir: completeDir, name: "failed-dir-retry",
	}
	s.fail.Store(true)
	s.start()

	s.kept = [2][]byte{
		[]byte(strings.Repeat("kept one ", 20)),
		[]byte(strings.Repeat("kept two ", 20)),
	}
	s.partial = [2][]byte{
		[]byte(strings.Repeat("partial 1 ", 20)),
		[]byte(strings.Repeat("partial 2 ", 20)),
	}
	fileXML := func(filename string, ids *[2]string, parts [2][]byte) string {
		var segs strings.Builder
		for i := range ids {
			ids[i] = randomMsgID(t)
			fmt.Fprintf(&segs, `<segment bytes="%d" number="%d">%s</segment>`+"\n",
				len(parts[i]), i+1, ids[i])
		}
		return fmt.Sprintf(`<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/2)">
<groups><group>alt.bin.test</group></groups>
<segments>
%s</segments>
</file>
`, filename, segs.String())
	}
	// The par2 index makes the missing article's damage repairable in
	// principle, so the job reaches post-processing rather than being
	// aborted as beyond repair, and finalize runs.
	par2ID := randomMsgID(t)
	par2Body := []byte("not really a par2 index")
	nzbXML := `<?xml version="1.0" encoding="iso-8859-1" ?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
` + fileXML("kept.bin", &s.keptIDs, s.kept) + fileXML("partial.bin", &s.partialIDs, s.partial) +
		fmt.Sprintf(`<file poster="p@t" date="1700000000" subject="&quot;%s.par2&quot; yEnc (1/1)">
<groups><group>alt.bin.test</group></groups>
<segments><segment bytes="%d" number="1">%s</segment></segments>
</file>
</nzb>
`, s.name, len(par2Body), par2ID)
	srv.AddArticle(par2ID, yencSinglePart(s.name+".par2", par2Body))
	keptTotal := int64(len(s.kept[0]) + len(s.kept[1]))
	for i := range s.keptIDs {
		srv.AddArticle(s.keptIDs[i], yencMultiPart("kept.bin", s.kept[i], i+1, 2, keptTotal))
	}
	partialTotal := int64(len(s.partial[0]) + len(s.partial[1]))
	// The second article of partial.bin is missing on the first attempt.
	srv.AddArticle(s.partialIDs[0], yencMultiPart("partial.bin", s.partial[0], 1, 2, partialTotal))

	parsed := mustParseNZB(t, []byte(nzbXML))
	j, hdr, err := app.BuildIngestJob(cfg, parsed, s.name+".nzb", types.FetchOptions{NzbName: s.name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := s.a.AddJob(t.Context(), j, hdr, []byte(nzbXML), false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.id = j.ID()
	s.waitStatus(constants.StatusFailed)

	failedDir := filepath.Join(downloadDir, "_FAILED_"+s.name)
	if _, err := os.Stat(filepath.Join(failedDir, "kept.bin")); err != nil {
		t.Fatalf("the first attempt left no kept.bin under %s: the finalize stage did not "+
			"rename the job directory, so this is not the scenario under test: %v", failedDir, err)
	}
	e, err := repo.Get(t.Context(), s.id)
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	if e.Path != failedDir {
		t.Fatalf("history path = %q, want the renamed directory %q", e.Path, failedDir)
	}
	return s
}

// start runs a new Application over the harness's directories and history
// database, with the verdict stage ahead of the real finalize stage.
func (s *failedDirRetry) start() {
	s.t.Helper()
	finalize := postproc.NewFinalizeStage()
	finalize.SetFolderRename(true)
	a, err := app.New(s.cfg, s.repo, app.WithPostProcStages([]postproc.Stage{
		failVerdictStage{fail: &s.fail},
		finalize,
	}))
	if err != nil {
		s.t.Fatal(err)
	}
	_, cancel := startAppAndDrain(s.t, a)
	var once sync.Once
	s.a = a
	s.stop = func() {
		once.Do(func() {
			if err := a.Shutdown(); err != nil {
				s.t.Errorf("Shutdown: %v", err)
			}
			cancel()
		})
	}
	s.t.Cleanup(s.stop)
}

func (s *failedDirRetry) restart() {
	s.t.Helper()
	s.stop()
	s.start()
}

// retry retries the job from history, waiting out the first attempt's
// finalizer if it still holds the job's transition claim.
func (s *failedDirRetry) retry() error {
	s.t.Helper()
	var err error
	waitUntil(recoveryLiveness, func() bool {
		err = s.a.RetryHistoryJob(s.t.Context(), s.id)
		return !errors.Is(err, app.ErrJobInTransition)
	})
	return err
}

// waitStatus waits for the job to leave the queue and be filed in history, and
// fails unless it was filed with status want. A retry deletes the entry it
// retried before it returns, so an entry found after one is the retry's own.
func (s *failedDirRetry) waitStatus(want constants.Status) {
	s.t.Helper()
	var e *history.Entry
	if !waitUntil(recoveryLiveness, func() bool {
		if _, queued := s.a.Dispatcher().Job(s.id); queued {
			return false
		}
		var gerr error
		e, gerr = s.repo.Get(s.t.Context(), s.id)
		return gerr == nil
	}) {
		s.t.Fatalf("job %s never reached history", s.id)
	}
	if e.Status != string(want) {
		s.t.Fatalf("job %s reached history as %q (fail message %q), want %s",
			s.id, e.Status, e.FailMessage, want)
	}
}

// TestRetryHistoryJob_ResumesInTheFailedDirectory: a retry of a job whose
// failed attempt the finalize stage renamed to _FAILED_<name> resumes in the
// bytes that attempt wrote. The finished job holds the file the first attempt
// completed and the article only the retry fetched, and no _FAILED_ directory
// is left behind.
func TestRetryHistoryJob_ResumesInTheFailedDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{name: "in process"},
		{name: "after a restart", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFailedDirRetry(t)
			if tc.restart {
				s.restart()
			}
			partialTotal := int64(len(s.partial[0]) + len(s.partial[1]))
			s.srv.AddArticle(s.partialIDs[1],
				yencMultiPart("partial.bin", s.partial[1], 2, 2, partialTotal))
			s.fail.Store(false)
			keptFetches := s.srv.FetchCount(s.keptIDs[0])

			if err := s.retry(); err != nil {
				t.Fatalf("RetryHistoryJob: %v", err)
			}
			s.waitStatus(constants.StatusCompleted)

			if n := s.srv.FetchCount(s.keptIDs[0]); n != keptFetches {
				t.Fatalf("the retry refetched an article the first attempt completed "+
					"(fetch count %d -> %d), so it did not resume", keptFetches, n)
			}
			finalDir := filepath.Join(s.completeDir, s.name)
			keptWant := append(append([]byte{}, s.kept[0]...), s.kept[1]...)
			if got, err := os.ReadFile(filepath.Join(finalDir, "kept.bin")); err != nil {
				t.Errorf("read kept.bin: %v", err)
			} else if !bytes.Equal(got, keptWant) {
				t.Errorf("kept.bin is %d bytes (%d of them zero), want the %d bytes the first "+
					"attempt downloaded", len(got), bytes.Count(got, []byte{0}), len(keptWant))
			}
			// partial.bin must hold the article the first attempt fetched at
			// offset 0. Whether the retry also refetches the one that attempt
			// missed turns on ResetForRetry reopening a file finalized short,
			// which this test does not pin; if it did, the file is both
			// articles in order.
			partialWant := append(append([]byte{}, s.partial[0]...), s.partial[1]...)
			if got, err := os.ReadFile(filepath.Join(finalDir, "partial.bin")); err != nil {
				t.Errorf("read partial.bin: %v", err)
			} else if !bytes.HasPrefix(got, s.partial[0]) ||
				(len(got) > len(s.partial[0]) && !bytes.Equal(got, partialWant)) {
				t.Errorf("partial.bin is %d bytes (%d of them zero), want its first article's "+
					"%d bytes, then its second article's %d if the retry fetched it",
					len(got), bytes.Count(got, []byte{0}), len(s.partial[0]), len(s.partial[1]))
			}
			if _, err := os.Lstat(filepath.Join(s.downloadDir, "_FAILED_"+s.name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the _FAILED_ directory outlived the retry (Lstat err %v)", err)
			}
		})
	}
}

// failedDirEntry files a FAILED history entry for a retryable job whose
// download directory the finalize stage renamed, with one file in it, and
// returns the application and the _FAILED_ and job directory paths.
func failedDirEntry(t *testing.T, id string, wrap func(checkpoint.Store) checkpoint.Store) (a *app.Application, failedDir, jobDir string) {
	t.Helper()
	a, repo, adminDir := newRetryTestApp(t)
	if wrap != nil {
		a.WrapCheckpointStore(wrap)
	}
	const name = "failedentry"
	downloadDir := a.Config().GetGeneral().DownloadDir
	jobDir = filepath.Join(downloadDir, name)
	failedDir = filepath.Join(downloadDir, "_FAILED_"+name)
	if err := os.Mkdir(failedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failedDir, "payload.bin"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGzNZB(t, adminDir, name+".nzb.gz", retryNZBWithRecoveryVolume(2, 1))
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      name,
		NzbName:   name + ".nzb",
		NZBBackup: name + ".nzb.gz",
		Status:    string(constants.StatusFailed),
		Path:      failedDir,
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return a, failedDir, jobDir
}

// TestRetryHistoryJob_AbortMovesTheFailedDirectoryBack: a retry that restored
// the _FAILED_ directory and then aborted moves it back to the path its history
// entry still records, so the entry stays retryable.
func TestRetryHistoryJob_AbortMovesTheFailedDirectoryBack(t *testing.T) {
	t.Parallel()
	const id = "failedabort00001"
	a, failedDir, jobDir := failedDirEntry(t, id, func(s checkpoint.Store) checkpoint.Store {
		return failForJobStore{Store: s, failID: id}
	})

	if err := a.RetryHistoryJob(t.Context(), id); !errors.Is(err, errJobCheckpoint) {
		t.Fatalf("RetryHistoryJob = %v, want the checkpoint write's error", err)
	}
	if _, err := os.Stat(filepath.Join(failedDir, "payload.bin")); err != nil {
		t.Errorf("the aborted retry did not move the directory back to %s: %v", failedDir, err)
	}
	if _, err := os.Lstat(jobDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the aborted retry left %s in place (Lstat err %v)", jobDir, err)
	}
}

// TestRetryHistoryJob_RefusesWhenTheJobDirectoryExists: a retry whose
// _FAILED_ directory cannot be moved back, because a directory already sits at
// the path the retry writes to, is refused and moves nothing.
func TestRetryHistoryJob_RefusesWhenTheJobDirectoryExists(t *testing.T) {
	t.Parallel()
	const id = "failedconflict01"
	a, failedDir, jobDir := failedDirEntry(t, id, nil)
	if err := os.Mkdir(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := a.RetryHistoryJob(t.Context(), id); !errors.Is(err, app.ErrRetryDirConflict) {
		t.Fatalf("RetryHistoryJob = %v, want the directory conflict", err)
	}
	if _, err := os.Stat(filepath.Join(failedDir, "payload.bin")); err != nil {
		t.Errorf("the refused retry moved the _FAILED_ directory: %v", err)
	}
	if _, held := a.Dispatcher().Job(id); held {
		t.Error("the refused retry queued the job")
	}
}
