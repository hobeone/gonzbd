package app_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestDurability_DoneMeansOnDisk verifies that an article flagged Done in the
// queue is actually on disk.
//
// It is deliberately end-to-end through the real barrier rather than through a
// test hook. The property is not "some code sets the bit in the right place";
// it is "by the time the bit is set, a completed fsync covers the bytes", and
// only the real chain — assembler write cache, barrier drain, fsync,
// RunStore commit, AckDurable — can demonstrate that. The previous version
// of this test replaced the whole ack path with a hook it supplied itself,
// which meant it pinned its own fixture.
//
// The file is held incomplete so the job stays in the queue to be inspected,
// and so the ack under test comes from the cadence rather than from the
// completion path.
func TestDurability_DoneMeansOnDisk(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	server := nntptest.New(t)
	payload := []byte("durability test article payload - bytes on stable storage")
	parsed := heldFile(t, server, "durable.bin", payload)

	srvCfg := server.ServerConfig("durability", 2)
	srvCfg.Timeout = 60 // the stall must outlast the assertions below
	a, err := app.New(testConfig(downloadDir, completeDir, adminDir, srvCfg), repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		app.WithCheckpointInterval(50*time.Millisecond),
		app.WithCheckpointBytes(1<<40),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
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

	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "durability-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Only AckDurable sets this bit, and only Barrier.Run and
	// Barrier.FinalizeFile can call it, because DurableProof has no
	// constructor outside internal/durability.
	if !waitUntil(10*time.Second, func() bool {
		snap := a.Queue().SnapshotJob(job.ID)
		return snap != nil && snap.Progress().ArticleDone(0)
	}) {
		t.Fatal("the article was never marked Done; nothing minted a DurableProof, " +
			"so every restart re-downloads it")
	}
	if runs := a.BarrierRuns(); runs == 0 {
		t.Error("the article is Done but no barrier ran — something acked outside the barrier (X2)")
	}

	snap := a.Queue().SnapshotJob(job.ID)
	filename := snap.Progress().FileFilename(0)
	if filename == "" {
		t.Fatal("the queue never recorded a resolved filename for file 0")
	}
	filePath := filepath.Join(downloadDir, "durability-job", filename)
	diskBytes, err := os.ReadFile(filePath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("the article is Done but its file is unreadable: %v", err)
	}
	if len(diskBytes) < len(payload) || !bytes.Equal(diskBytes[:len(payload)], payload) {
		t.Errorf("the article is marked Done but the file on disk does not hold its bytes:\n got  %q\n want %q",
			diskBytes, payload)
	}

	// The bit must reach the file too: an in-memory Done that never lands is
	// exactly the state a restart cannot use.
	if err := a.Queue().Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("Queue.Save: %v", err)
	}
	reloaded := loadTestQueue(t, repo, adminDir).SnapshotJob(job.ID)
	if reloaded == nil {
		t.Fatal("job missing from the on-disk queue")
	}
	if !reloaded.Progress().ArticleDone(0) {
		t.Error("the article is Done in memory but not on disk")
	}
}

// TestDurability_AcceptedIsNotDone pins S2 from the other side: entry into a
// buffer, a channel, or the assembler's write cache is never evidence about
// disk, so an article the downloader has delivered and decoded must stay
// Outstanding until a barrier has fsynced it.
//
// This is the half a completed download cannot show. In the test above the
// barrier runs almost immediately, so "delivered" and "Done" are
// indistinguishable; here the cadence is pinned open — an hour-long interval
// and a byte bound no fixture can cross — and the file is kept incomplete, so
// nothing triggers a barrier at all. The article is provably received and
// provably not Done.
//
// Without this, moving the ack back to the accept path (defect #355, refiled
// as #356) passes every other test in this package.
func TestDurability_AcceptedIsNotDone(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	server := nntptest.New(t)
	parsed := heldFile(t, server, "held.bin", []byte("first article bytes"))
	srvCfg := server.ServerConfig("accepted-not-done", 2)
	srvCfg.Timeout = 60 // the stall must outlast the assertions below

	a, err := app.New(testConfig(downloadDir, completeDir, adminDir, srvCfg), repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		// An hour-long interval and a terabyte-sized byte bound mean neither
		// half of B1 can fire during this test.
		app.WithCheckpointInterval(time.Hour),
		app.WithCheckpointBytes(1<<40),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		// ForceStopWorkers, not Shutdown: Shutdown runs R6's clean-shutdown
		// barrier, which would ack the very article this test is about.
		a.ForceStopWorkers()
		cancel()
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())

	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "accepted-not-done"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}
	awaitFirstArticle(t, a, job.ID)

	if runs := a.BarrierRuns(); runs != 0 {
		t.Fatalf("%d barriers ran despite an hour-long interval and a 1 TiB byte bound; "+
			"the article below may legitimately be Done and this test proves nothing", runs)
	}
	snap := a.Queue().SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job left the queue")
	}
	if snap.Progress().ArticleDone(0) {
		t.Error("article 0 is marked Done with no fsync behind it — acceptance into a " +
			"buffer was treated as evidence about disk (S2, #355)")
	}
	if snap.Progress().ArticleDone(1) {
		t.Error("article 1 is marked Done although it never arrived at all")
	}
}

// TestBarrierFiresOnByteBound pins B1's volume half.
//
// The interval is set to an hour so the time bound cannot fire, which is what
// makes a barrier run attributable to the byte bound alone. Without that
// separation the test passes whichever bound is wired and neither, if the
// default 30s tick happened to land.
func TestBarrierFiresOnByteBound(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	server := nntptest.New(t)
	const n = 6
	payload := bytes.Repeat([]byte("x"), 512)
	files := make([]nzb.File, 0, n)
	for i := range n {
		msgID := randomMsgID(t)
		name := fmt.Sprintf("bytebound%d.bin", i)
		server.AddArticle(msgID, yencSinglePart(name, payload))
		files = append(files, nzb.File{
			Subject:  fmt.Sprintf(`"%s" yEnc (1/1)`, name),
			Articles: []nzb.Article{{ID: msgID, Bytes: len(payload), Number: 1}},
			Bytes:    int64(len(payload)),
		})
	}

	a, err := app.New(testConfig(downloadDir, completeDir, adminDir,
		server.ServerConfig("byte-bound", 2), //nolint:gocritic // positional args match testConfig
	), repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		app.WithCheckpointInterval(time.Hour),
		// One article's payload is 512 bytes, so this bound is crossed after
		// two of them and well before the six the job holds.
		app.WithCheckpointBytes(1024),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
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

	job, err := queue.NewJob(&nzb.NZB{Files: files},
		queue.AddOptions{Name: "byte-bound"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	if !waitUntil(10*time.Second, func() bool { return a.BarrierRuns() >= 1 }) {
		t.Fatal("no barrier ran after the byte bound was crossed; with an hour-long " +
			"interval the volume bound is the only thing that can fire, so a fast link " +
			"would carry a whole interval's downloads unacked")
	}
}

// TestBarrierFiresOnTimeBound pins B1's time half, with the byte bound set
// beyond anything the fixture can reach so a run is attributable to the ticker
// alone.
func TestBarrierFiresOnTimeBound(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	server := nntptest.New(t)
	parsed := heldFile(t, server, "timebound.bin", []byte("time bound"))
	srvCfg := server.ServerConfig("time-bound", 2)
	srvCfg.Timeout = 60

	a, err := app.New(testConfig(downloadDir, completeDir, adminDir, srvCfg), repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		app.WithCheckpointInterval(50*time.Millisecond),
		app.WithCheckpointBytes(1<<40),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
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

	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "time-bound"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Two runs, not one: a single run could come from any one-off trigger,
	// while a second proves the ticker keeps firing.
	if !waitUntil(10*time.Second, func() bool { return a.BarrierRuns() >= 2 }) {
		t.Fatalf("barrier ran %d times in 10s at a 50ms interval with a 1 TiB byte bound; "+
			"the time bound is not driving the cadence", a.BarrierRuns())
	}
}

// TestBarrierRunsOnCleanShutdown pins R6's shutdown trigger.
//
// A deliberate restart must not cost a checkpoint window's worth of
// re-downloading. The cadence is pinned open — an hour and a terabyte — so the
// only thing that can produce a run is Shutdown itself.
func TestBarrierRunsOnCleanShutdown(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	server := nntptest.New(t)
	parsed := heldFile(t, server, "shutdown.bin", []byte("shutdown barrier"))
	srvCfg := server.ServerConfig("shutdown-barrier", 2)
	srvCfg.Timeout = 60

	a, err := app.New(testConfig(downloadDir, completeDir, adminDir, srvCfg), repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		app.WithCheckpointInterval(time.Hour),
		app.WithCheckpointBytes(1<<40),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())

	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "shutdown-barrier"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}
	awaitFirstArticle(t, a, job.ID)

	if before := a.BarrierRuns(); before != 0 {
		t.Fatalf("%d barriers ran before shutdown despite the cadence being pinned open; "+
			"the assertion below would not attribute anything to shutdown", before)
	}
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if a.BarrierRuns() == 0 {
		t.Fatal("shutdown ran no final barrier; everything downloaded since the last " +
			"checkpoint is re-fetched on the next start (R6)")
	}

	// And the ack it produced is on disk, not just in memory: Shutdown saves
	// the queue after the barrier for exactly this reason.
	snap := loadTestQueue(t, repo, adminDir).SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job missing from the on-disk queue after shutdown")
	}
	if !snap.Progress().ArticleDone(0) {
		t.Error("the shutdown barrier acked article 0 but the queue was saved before it, " +
			"so the ack did not survive the process")
	}
}

// heldFile builds a two-part file whose second part stalls forever, and
// registers both parts with the server.
//
// The stall is what keeps the file INCOMPLETE, and that is the point: a
// completed file triggers Barrier.FinalizeFile, so a test that let its file
// complete could not attribute a barrier to the cadence it was trying to pin.
// A missing second article would not do — it fails permanently, still counts
// toward the file's part total, and completes the file with a hole in it.
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

// awaitFirstArticle blocks until the job has recorded downloaded bytes.
//
// ServerStats is charged in the pipeline immediately after a successful decode
// and before the article reaches the assembler, so a non-zero figure is proof
// the article was received — which is exactly the state that must NOT be
// mistaken for durability.
func awaitFirstArticle(t *testing.T, a *app.Application, jobID string) {
	t.Helper()
	if !waitUntil(10*time.Second, func() bool {
		snap := a.Queue().SnapshotJob(jobID)
		if snap == nil {
			return false
		}
		var total int64
		for _, n := range snap.Progress().ServerStats() {
			total += n
		}
		return total > 0
	}) {
		t.Fatal("the first article was never received; the fixture proves nothing about what follows")
	}
}
