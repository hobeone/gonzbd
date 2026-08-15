package app

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// duFixture builds an Application wired with just enough for maybeStart:
// a queue holding one RAR-volume job, a pipeline that can resolve the file's
// on-disk path, and config.
//
// The volume is deliberately part02. AnalyzeRarFilename reports it as volume
// 2, so DirectUnpacker.Add records it and queues the set rather than starting
// extraction — volume 1 is what triggers a start. That keeps these tests on
// the orchestrator's own logic instead of spawning an extraction.
func duFixture(t *testing.T, threads int) (*directUnpackOrchestrator, *queue.Queue, *queue.Job) {
	t.Helper()
	o, q, j, _ := duFixtureLogged(t, threads)
	return o, q, j
}

// duFixtureLogged is duFixture plus the captured log, for the one test that
// asserts on a decision the orchestrator only reports by logging.
func duFixtureLogged(t *testing.T, threads int) (*directUnpackOrchestrator, *queue.Queue, *queue.Job, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer

	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.part02.rar", Bytes: 200, Articles: []nzb.Article{
			{ID: "v2a0@t", Bytes: 100, Number: 1},
			{ID: "v2a1@t", Bytes: 100, Number: 2},
		}},
	}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "movie.nzb", PP: 3}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	dir := t.TempDir()
	cfg.With(func(c *config.Config) {
		c.General.DownloadDir = dir
		c.PostProc.DirectUnpackThreads = threads
	})

	app := &Application{
		queue:   q,
		log:     slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		config:  cfg,
		emitter: dummyEmitter{},
		ctx:     context.Background(),
	}
	app.pipeline = &pipeline{
		log:         app.log,
		queue:       q,
		downloadDir: dir,
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}
	// resolveFileInfo reads this map; without an entry maybeStart returns
	// before ever reaching the orchestrator's own logic.
	if err := app.pipeline.registerFile(job.ID, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}

	o := newDirectUnpackOrchestrator(app)
	app.duOrch = o
	return o, q, job, &logBuf
}

func (o *directUnpackOrchestrator) countsForTest() (unpackers, active int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.unpackers), o.active
}

// The concurrency counter is shared mutable state and nothing proved it was
// accounted correctly: one unpacker per job, created once, counted once.
func TestMaybeStart_CreatesOneUnpackerPerJobAndCountsIt(t *testing.T) {
	o, _, job := duFixture(t, 0) // 0 = unlimited

	o.maybeStart(FileComplete{JobID: job.ID, FileIdx: 0})
	n, active := o.countsForTest()
	if n != 1 || active != 1 {
		t.Fatalf("after first call: unpackers=%d active=%d, want 1 and 1", n, active)
	}

	// A second completed file on the same job must feed the existing
	// unpacker, not create a second one or double-count the slot.
	o.maybeStart(FileComplete{JobID: job.ID, FileIdx: 0})
	n, active = o.countsForTest()
	if n != 1 || active != 1 {
		t.Errorf("after second call: unpackers=%d active=%d, want 1 and 1 — the job was counted twice against the limit", n, active)
	}
}

// With the limit already reached, no unpacker is created and the counter is
// left alone. Incrementing here would leak a slot no unpacker ever releases.
func TestMaybeStart_RefusesWhenConcurrencyLimitReached(t *testing.T) {
	o, _, job := duFixture(t, 1)
	o.setActive(1) // the one permitted slot is taken by another job

	o.maybeStart(FileComplete{JobID: job.ID, FileIdx: 0})

	n, active := o.countsForTest()
	if n != 0 {
		t.Errorf("created %d unpacker(s) despite the limit being reached", n)
	}
	if active != 1 {
		t.Errorf("active = %d, want 1 — the refused start must not consume a slot", active)
	}
}

// A file can complete with permanently failed articles: the on-disk volume is
// the right size but has gaps. Extraction must not treat it as sound, so the
// set is marked corrupt before the volume is handed over.
func TestMaybeStart_MarksSetCorruptWhenTheVolumeHasFailedArticles(t *testing.T) {
	o, q, job, logBuf := duFixtureLogged(t, 0)

	ackFailed(t, q, job.ID, "v2a1@t")

	o.maybeStart(FileComplete{JobID: job.ID, FileIdx: 0})

	o.mu.Lock()
	du := o.unpackers[job.ID]
	o.mu.Unlock()
	if du == nil {
		t.Fatal("no unpacker was created")
	}
	// Assert the effect, not the log line beside it: an earlier version of
	// this test checked only the log and kept passing with the MarkCorrupt
	// call deleted.
	if _, corrupt := du.CorruptSets()["movie"]; !corrupt {
		t.Errorf("set was not marked corrupt despite the volume having a failed article; extraction would trust incomplete data.\nlog: %s", logBuf.String())
	}
}

// The guards before any state is touched. Each returns early, and none may
// create an unpacker or move the counter.
func TestMaybeStart_IneligibleJobsCreateNothing(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, q *queue.Queue, job *queue.Job)
		fileIdx int
	}{
		{name: "file index out of range", fileIdx: 99},
		{name: "negative file index", fileIdx: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, q, job := duFixture(t, 0)
			if tc.mutate != nil {
				tc.mutate(t, q, job)
			}
			o.maybeStart(FileComplete{JobID: job.ID, FileIdx: tc.fileIdx})
			if n, active := o.countsForTest(); n != 0 || active != 0 {
				t.Errorf("unpackers=%d active=%d, want 0 and 0", n, active)
			}
		})
	}

	t.Run("unknown job", func(t *testing.T) {
		o, _, _ := duFixture(t, 0)
		o.maybeStart(FileComplete{JobID: "no-such-job", FileIdx: 0})
		if n, active := o.countsForTest(); n != 0 || active != 0 {
			t.Errorf("unpackers=%d active=%d, want 0 and 0", n, active)
		}
	})
}
