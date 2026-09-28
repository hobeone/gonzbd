package app_test

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestFail_AJobInPostProcessingIsNotDispatched: Application.Fail hands a job
// to post-processing without moving it off Fetching, so its row still reads
// Running, Fetching and IntentRun until the finalizer cancels it. The
// downloader must not fetch its articles in that window: its handles are
// closed, and a write would create a file under a running post-processor.
//
// The sentinel job is what makes the negative assertion mean something: it is
// queued behind the failed job, so a downloader that fetched it had the
// failed job's row in front of it, and the last check confirms that row was
// dispatchable by its own fields.
//
// A failed job skips every stage, so the window is the time it waits in the
// post-processing queue behind another job.
func TestFail_AJobInPostProcessingIsNotDispatched(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	srv := nntptest.New(t)
	cfg := testConfig(downloadDir, completeDir, adminDir, srv.ServerConfig("handoff", 2))
	stage := cancelHonouringStage{entered: make(chan string, 2)}
	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{stage}))
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := startAppAndDrain(t, a)
	t.Cleanup(func() { cancel(); _ = a.Shutdown() })

	addJob := func(name string) (*job.Job, string) {
		t.Helper()
		msgID := randomMsgID(t)
		raw := []byte("payload for " + name)
		srv.AddArticle(msgID, yencSinglePart(name+".bin", raw))
		parsed := &nzb.NZB{Files: []nzb.File{{
			Subject:  fmt.Sprintf(`"%s.bin" yEnc (1/1)`, name),
			Articles: []nzb.Article{{ID: msgID, Bytes: len(raw), Number: 1}},
			Bytes:    int64(len(raw)),
		}}}
		j, hdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: name})
		if err := a.Dispatcher().Add(t.Context(), j, hdr); err != nil {
			t.Fatalf("Add(%s): %v", name, err)
		}
		return j, msgID
	}

	// The post-processor runs one job at a time. Occupying it holds the failed
	// job in its queue, which is where a real one waits behind a long repair.
	blocker, _ := addJob("handoff-blocker")
	if got := awaitEntered(t, stage.entered); got != blocker.ID() {
		t.Fatalf("stage entered for %s, want the blocker %s", got, blocker.ID())
	}

	// Paused so nothing is fetched before the hand-off.
	a.Dispatcher().Pause()
	failed, failedMsg := addJob("handoff-failed")
	dir := filepath.Join(downloadDir, failed.Name())
	a.Fail(failed.ID(), storagefault.Classify("write", filepath.Join(dir, "x.bin"), syscall.EFBIG))
	if !waitUntil(recoveryLiveness, func() bool { return a.PostProcessorHas(failed.ID()) }) {
		t.Fatal("the failed job never reached the post-processing queue")
	}
	sentinel, _ := addJob("handoff-sentinel")
	a.Dispatcher().Resume()

	// The sentinel reaching post-processing means its download finished, and
	// the dispatch passes that fetched it visited the failed job's row too.
	if !waitUntil(recoveryLiveness, func() bool { return a.PostProcessorHas(sentinel.ID()) }) {
		t.Fatal("the sentinel job never finished downloading, so the downloader never ran a dispatch pass")
	}

	if n := srv.FetchCount(failedMsg); n != 0 {
		t.Errorf("the failed job's article was fetched %d time(s) while it was in "+
			"post-processing; its handles are closed, so the bytes are either "+
			"discarded or written into a file the post-processor is working on", n)
	}
	// The job wrote nothing before the hand-off, so anything here was created
	// under the post-processor.
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		t.Errorf("the failed job's download directory holds %d file(s), first %q: a "+
			"file was created while the job was in post-processing", len(ents), ents[0].Name())
	}
	// The row must still look dispatchable, or the check above passed because
	// something else kept the job off the wire.
	row, ok := a.Dispatcher().Row(failed.ID())
	if !ok || !row.View.Running || row.View.State != job.Fetching || row.View.Intent != job.IntentRun {
		t.Errorf("failed job's row = %v %+v, want Running at Fetching with IntentRun", ok, row.View)
	}
}
