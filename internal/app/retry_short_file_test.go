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
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// shortFileRetry is a job of a two-article file and a par2 index whose first
// article is missing from the server on its first attempt. That attempt
// finalizes the file short, and its repair stage fails the job into history.
//
// The par2 index is what lets the file finalize: it makes the damage's
// repairability unknown rather than hopeless, so the job is not failed while
// the file is still missing parts.
type shortFileRetry struct {
	t           *testing.T
	cfg         *config.Config
	a           *app.Application
	stop        func()
	repo        *history.Repository
	srv         *nntptest.Scripted
	id          string
	downloadDir string
	name        string
	msgIDs      [2]string
	parts       [2][]byte
	// repairFails is what the post-processing stage reports: set, it fails the
	// job the way a par2 repair that cannot fix a short file does.
	repairFails atomic.Bool
}

// repairVerdictStage stands in for par2 repair: it fails the job while fail is
// set and passes it otherwise, and does no work either way.
type repairVerdictStage struct{ fail *atomic.Bool }

func (repairVerdictStage) Name() string { return "repair-verdict" }

func (s repairVerdictStage) Run(_ context.Context, j *postproc.Job) error {
	if s.fail.Load() {
		j.ParError = true
	}
	return nil
}

func newShortFileRetry(t *testing.T) *shortFileRetry {
	t.Helper()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	srv := nntptest.New(t)
	cfg := testConfig(downloadDir, completeDir, adminDir, srv.ServerConfig("retry", 2))
	s := &shortFileRetry{t: t, cfg: cfg, repo: repo, srv: srv, downloadDir: downloadDir, name: "short-retry"}
	s.repairFails.Store(true)
	s.start()
	a := s.a
	s.parts = [2][]byte{
		[]byte(strings.Repeat("first part ", 20)),
		[]byte(strings.Repeat("second part", 20)),
	}
	var segs strings.Builder
	for i := range s.msgIDs {
		s.msgIDs[i] = randomMsgID(t)
		fmt.Fprintf(&segs, `<segment bytes="%d" number="%d">%s</segment>`+"\n",
			len(s.parts[i]), i+1, s.msgIDs[i])
	}
	par2ID := randomMsgID(t)
	par2Body := []byte("not really a par2 index")
	nzbXML := fmt.Sprintf(`<?xml version="1.0" encoding="iso-8859-1" ?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<file poster="p@t" date="1700000000" subject="&quot;%s.bin&quot; yEnc (1/2)">
<groups><group>alt.bin.test</group></groups>
<segments>
%s</segments>
</file>
<file poster="p@t" date="1700000000" subject="&quot;%s.par2&quot; yEnc (1/1)">
<groups><group>alt.bin.test</group></groups>
<segments><segment bytes="%d" number="1">%s</segment></segments>
</file>
</nzb>
`, s.name, segs.String(), s.name, len(par2Body), par2ID)
	srv.AddArticle(par2ID, yencSinglePart(s.name+".par2", par2Body))

	total := int64(len(s.parts[0]) + len(s.parts[1]))
	// Only the second article exists on the first attempt.
	srv.AddArticle(s.msgIDs[1], yencMultiPart(s.name+".bin", s.parts[1], 2, 2, total))

	parsed := mustParseNZB(t, []byte(nzbXML))
	j, hdr, err := app.BuildIngestJob(cfg, parsed, s.name+".nzb", types.FetchOptions{NzbName: s.name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := a.AddJob(t.Context(), j, hdr, []byte(nzbXML), false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.id = j.ID()
	s.waitStatus(constants.StatusFailed)
	if n := srv.FetchCount(s.msgIDs[0]); n == 0 {
		t.Fatal("the first attempt never asked for the missing article")
	}
	retained, err := repo.RetainedFiles(t.Context(), s.id)
	if err != nil {
		t.Fatalf("RetainedFiles: %v", err)
	}
	if len(retained) == 0 || retained[0].FileIndex != 0 || !retained[0].Complete {
		t.Fatalf("retained file progress = %+v, want file 0 recorded complete: the first "+
			"attempt did not finalize it short, so this is not the scenario under test",
			retained)
	}
	return s
}

// start runs a new Application over the harness's directories and history
// database.
func (s *shortFileRetry) start() {
	s.t.Helper()
	a, err := app.New(s.cfg, s.repo, app.WithPostProcStages([]postproc.Stage{
		repairVerdictStage{fail: &s.repairFails},
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

// restart shuts the Application down cleanly and starts a new one, so that
// nothing the first attempt left in memory survives into the retry.
func (s *shortFileRetry) restart() {
	s.t.Helper()
	s.stop()
	s.start()
}

// addMissingArticle makes the article the first attempt could not fetch
// available on the server.
func (s *shortFileRetry) addMissingArticle() {
	total := int64(len(s.parts[0]) + len(s.parts[1]))
	s.srv.AddArticle(s.msgIDs[0], yencMultiPart(s.name+".bin", s.parts[0], 1, 2, total))
}

// retry retries the job from history. The first attempt's finalizer can still
// hold the job's transition claim after its history entry is visible, and a
// retry refuses rather than waits for it, so a refusal on that ground is
// retried until the claim is released.
func (s *shortFileRetry) retry() {
	s.t.Helper()
	var err error
	waitUntil(recoveryLiveness, func() bool {
		err = s.a.RetryHistoryJob(s.t.Context(), s.id)
		return !errors.Is(err, app.ErrJobInTransition)
	})
	if err != nil {
		s.t.Fatalf("RetryHistoryJob: %v", err)
	}
}

// waitStatus waits for the job to leave the queue and be filed in history with
// status want.
func (s *shortFileRetry) waitStatus(want constants.Status) {
	s.t.Helper()
	var got string
	if !waitUntil(recoveryLiveness, func() bool {
		if _, queued := s.a.Dispatcher().Job(s.id); queued {
			return false
		}
		e, gerr := s.repo.Get(s.t.Context(), s.id)
		if gerr != nil {
			return false
		}
		got = e.Status
		return got == string(want)
	}) {
		s.t.Fatalf("job %s never reached history as %s (last status %q)", s.id, want, got)
	}
}

// TestRetryHistoryJob_RefetchesTheArticleAShortFileMissed: a retry of a job
// whose first attempt finalized a file short fetches the article that attempt
// could not, and the file ends up holding every article's bytes. The retained
// history row still records the file as complete, since the first attempt did
// finalize it; the retry must not take that as meaning there is nothing left
// to fetch. That row is read from the history database, so a retry after a
// restart inherits it just as an in-process one does.
func TestRetryHistoryJob_RefetchesTheArticleAShortFileMissed(t *testing.T) {
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
			s := newShortFileRetry(t)
			if tc.restart {
				s.restart()
			}
			s.retryDownloadsTheMissedArticle()
		})
	}
}

func (s *shortFileRetry) retryDownloadsTheMissedArticle() {
	t := s.t
	t.Helper()
	before := s.srv.FetchCount(s.msgIDs[0])
	s.addMissingArticle()
	s.repairFails.Store(false)

	s.retry()
	s.waitStatus(constants.StatusCompleted)

	if n := s.srv.FetchCount(s.msgIDs[0]); n == before {
		t.Errorf("the retry never fetched the article the first attempt missed "+
			"(fetch count still %d)", n)
	}
	got, err := os.ReadFile(filepath.Join(s.downloadDir, s.name, s.name+".bin"))
	if err != nil {
		t.Fatalf("read the retried file: %v", err)
	}
	want := append(append([]byte{}, s.parts[0]...), s.parts[1]...)
	if !bytes.Equal(got, want) {
		t.Errorf("the retried file is %d bytes (%d of them zero), want the %d bytes of both "+
			"articles", len(got), bytes.Count(got, []byte{0}), len(want))
	}
}

// TestRetryHistoryJob_ReattemptsAnArticleStillMissing: a retry asks the server
// again for an article that is still missing, rather than taking the short file
// as finished. The job then fails again because its repair still fails; whether
// a short file passes post-processing is that stage's verdict, not the retry's.
func TestRetryHistoryJob_ReattemptsAnArticleStillMissing(t *testing.T) {
	t.Parallel()
	s := newShortFileRetry(t)
	before := s.srv.FetchCount(s.msgIDs[0])

	s.retry()
	s.waitStatus(constants.StatusFailed)

	if n := s.srv.FetchCount(s.msgIDs[0]); n == before {
		t.Errorf("the retry never re-attempted the missing article (fetch count still %d)", n)
	}
}
