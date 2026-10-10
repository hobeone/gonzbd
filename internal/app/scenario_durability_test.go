package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// startHeldApp starts an application over a held file (see heldFile) with the
// given record interval, and returns it with the job it added.
func startHeldApp(t *testing.T, name string, interval time.Duration) (*app.Application, *history.Repository, string, string) {
	t.Helper()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	server := nntptest.New(t)
	payload := []byte(name + " article payload - bytes in the file")
	parsed := heldFile(t, server, name+".bin", payload)
	srvCfg := server.ServerConfig(name, 2)
	srvCfg.Timeout = 60 // the stall must outlast the assertions

	cfg := testConfig(downloadDir, completeDir, adminDir, srvCfg)
	a, err := app.New(cfg, repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		app.WithRecordInterval(interval),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())

	job, hdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: name + ".nzb"})
	if err := a.AddJob(t.Context(), job, hdr, []byte("<nzb/>"), false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		j, ok := a.Dispatcher().Job(job.ID())
		return ok && j.Progress().ArticleDone(0)
	}) {
		t.Fatal("article 0 was never marked Done")
	}
	return a, repo, filepath.Join(downloadDir, job.Name()), job.ID()
}

// writtenArticles reports the article indexes written_articles holds for jobID.
func writtenArticles(t *testing.T, repo *history.Repository, jobID string) []int32 {
	t.Helper()
	rows, err := durability.NewStore(repo.DB()).WrittenRows(t.Context(), jobID)
	if err != nil {
		t.Fatalf("WrittenRows: %v", err)
	}
	out := make([]int32, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ArtIdx)
	}
	return out
}

// TestDurability_DoneMeansWrittenAndRecorded verifies, end to end, that an
// article flagged Done has its bytes in the file and reaches written_articles
// on the recorder's interval. The file is held incomplete so the job stays in
// the queue, and so the row comes from the interval flush rather than from
// the completion path.
func TestDurability_DoneMeansWrittenAndRecorded(t *testing.T) {
	t.Parallel()
	a, repo, jobDir, id := startHeldApp(t, "recorded", 50*time.Millisecond)
	t.Cleanup(func() { a.StopAndJoin(t) })

	j, ok := a.Dispatcher().Job(id)
	if !ok {
		t.Fatal("job not in dispatcher")
	}
	filename := j.Progress().FileFilename(0)
	if filename == "" {
		t.Fatal("the queue never recorded a resolved filename for file 0")
	}
	payload := []byte("recorded article payload - bytes in the file")
	diskBytes, err := os.ReadFile(filepath.Join(jobDir, filename)) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("the article is Done but its file is unreadable: %v", err)
	}
	if len(diskBytes) < len(payload) || !bytes.Equal(diskBytes[:len(payload)], payload) {
		t.Errorf("the article is marked Done but the file does not hold its bytes:\n got  %q\n want %q",
			diskBytes, payload)
	}

	if !waitUntil(10*time.Second, func() bool { return len(writtenArticles(t, repo, id)) > 0 }) {
		t.Fatal("the article is Done in memory but no written_articles row reached SQLite " +
			"on a 50ms record interval")
	}
	if got := writtenArticles(t, repo, id); len(got) != 1 || got[0] != 0 {
		t.Errorf("written_articles = %v, want [0]", got)
	}
}

// TestRecord_FlushesOnCleanShutdown pins the shutdown flush: a deliberate
// restart must not cost a record interval's worth of re-downloading. The
// interval is an hour, so the only thing that can write the row is Shutdown.
func TestRecord_FlushesOnCleanShutdown(t *testing.T) {
	t.Parallel()
	a, repo, _, id := startHeldApp(t, "shutdown", time.Hour)

	if got := writtenArticles(t, repo, id); len(got) != 0 {
		t.Fatalf("written_articles = %v before shutdown with an hour-long interval; "+
			"the assertion below would not attribute anything to shutdown", got)
	}
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := writtenArticles(t, repo, id); len(got) != 1 || got[0] != 0 {
		t.Errorf("written_articles = %v after shutdown, want [0]: everything written "+
			"since the last flush is re-fetched on the next start", got)
	}
}

// heldFile builds a two-part file whose second part stalls forever, and
// registers both parts with the server.
//
// The stall is what keeps the file INCOMPLETE: a completed file is flushed
// through the completion path, so a test that let its file complete could not
// attribute a row to the interval or to shutdown. A missing second article
// would not do — it fails permanently, still counts toward the file's part
// total, and completes the file with a hole in it.
//
// The server is given two connections so the dispatcher cannot spend its only
// one on the stalled article and never fetch the first.
func heldFile(t *testing.T, server *nntptest.Scripted, name string, payload []byte) *nzb.NZB {
	t.Helper()
	total := int64(len(payload)) * 2
	first, second := randomMsgID(t), randomMsgID(t)
	server.AddArticle(first, yencMultiPart(name, payload, 1, 2, total))
	server.AddArticle(second, yencMultiPart(name, payload, 2, 2, total))
	server.InjectFailure(second, nntptest.FailureStall)
	return &nzb.NZB{Files: []nzb.File{{
		Subject: `"` + name + `" yEnc (1/2)`,
		Articles: []nzb.Article{
			{ID: first, Bytes: len(payload), Number: 1},
			{ID: second, Bytes: len(payload), Number: 2},
		},
		Bytes: total,
	}}}
}
