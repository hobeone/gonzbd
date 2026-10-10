package app_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestRetryHistoryJob_AbortsWhenTheAssemblerCannotForgetTheJob: the job-level
// tombstone set when a job entered post-processing drops every article of a
// retry under its ID without resolving it, so a retry whose ForgetJob failed
// would sit at Fetching forever. It must fail instead, leaving nothing queued.
func TestRetryHistoryJob_AbortsWhenTheAssemblerCannotForgetTheJob(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	if err := application.ForceAssemblerStopped(); err != nil {
		t.Fatalf("ForceAssemblerStopped: %v", err)
	}

	const id = "retryforgetfails"
	writeGzNZB(t, adminDir, "forgetfails.nzb.gz", retryNZB(2))
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "forgetfails",
		NzbName:   "forgetfails.nzb",
		NZBBackup: "forgetfails.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	err := application.RetryHistoryJob(t.Context(), id)
	if !errors.Is(err, assembler.ErrStopped) {
		t.Fatalf("RetryHistoryJob = %v, want the assembler's ErrStopped: a retry that "+
			"keeps the job's tombstone never makes progress", err)
	}
	if _, queued := application.Dispatcher().Job(id); queued {
		t.Error("the aborted retry is in the queue")
	}
	path, perr := app.ManifestPath(adminDir, id)
	if perr != nil {
		t.Fatalf("ManifestPath: %v", perr)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("stat of the aborted retry's manifest = %v, want not-exist", serr)
	}
}

// TestRetryHistoryJob_InProcessRetryDownloads: a job that failed in this
// process and is retried in this process downloads. Entering post-processing
// left the assembler's job-level tombstone behind, and the retry's writes land
// only because ForgetJob clears it.
//
// The first run fails through Application.Fail before anything is fetched, so
// the retry has a whole file to write and nothing retained to confuse it.
func TestRetryHistoryJob_InProcessRetryDownloads(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	srv := nntptest.New(t)
	cfg := testConfig(downloadDir, completeDir, adminDir, srv.ServerConfig("retry", 2))
	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := startAppAndDrain(t, a)
	t.Cleanup(func() { cancel(); _ = a.Shutdown() })

	msgID := randomMsgID(t)
	name := "inprocess-retry"
	raw := []byte(strings.Repeat("retry payload ", 20))
	nzbXML := fmt.Sprintf(`<?xml version="1.0" encoding="iso-8859-1" ?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<file poster="p@t" date="1700000000" subject="&quot;%s.bin&quot; yEnc (1/1)">
<groups><group>alt.bin.test</group></groups>
<segments><segment bytes="%d" number="1">%s</segment></segments>
</file>
</nzb>
`, name, len(raw), msgID)
	parsed := mustParseNZB(t, []byte(nzbXML))
	j, hdr, err := app.BuildIngestJob(cfg, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	srv.AddArticle(msgID, yencSinglePart(name+".bin", raw))
	a.Dispatcher().Pause()
	if err := a.AddJob(t.Context(), j, hdr, []byte(nzbXML), false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	id := j.ID()
	a.Fail(id, storagefault.Classify("write", filepath.Join(downloadDir, name), syscall.EACCES))
	waitStatus := func(want constants.Status) {
		t.Helper()
		var got string
		if !waitUntil(recoveryLiveness, func() bool {
			if _, queued := a.Dispatcher().Job(id); queued {
				return false
			}
			e, gerr := repo.Get(t.Context(), id)
			if gerr != nil {
				return false
			}
			got = e.Status
			return got == string(want)
		}) {
			t.Fatalf("job %s never reached history as %s (last status %q)", id, want, got)
		}
	}
	waitStatus(constants.StatusFailed)

	if n := srv.FetchCount(msgID); n != 0 {
		t.Fatalf("the first run fetched the article %d time(s); the retry has nothing left to prove", n)
	}

	// The history entry and the dispatcher removal both land inside the
	// finalizer's persistAndCommit, which holds the job's transition lock
	// until it returns, so a retry here can be refused as errJobInTransition.
	// That refusal is taken before RetryHistoryJob acts on anything, and the
	// lock has no other observable, so poll it until the finalizer lets go.
	var retryErr error
	if !waitUntil(recoveryLiveness, func() bool {
		retryErr = a.RetryHistoryJob(t.Context(), id)
		return !errors.Is(retryErr, app.ErrJobInTransition)
	}) {
		t.Fatalf("the finalizer never released the job's transition lock: %v", retryErr)
	}
	if retryErr != nil {
		t.Fatalf("RetryHistoryJob: %v", retryErr)
	}
	a.Dispatcher().Resume()
	waitStatus(constants.StatusCompleted)
	got, err := os.ReadFile(filepath.Join(downloadDir, name, name+".bin"))
	if err != nil {
		t.Fatalf("read the retried file: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("the retried file's %d bytes are not the article's payload (%d of them "+
			"zero): the retry's write never landed", len(got), bytes.Count(got, []byte{0}))
	}
}
