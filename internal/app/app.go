// Package app wires the download pipeline: queue → downloader → decoder →
// assembler. It owns the lifecycle of each subsystem (Start, Shutdown) and
// bridges between them via a pipeline goroutine that decodes raw NNTP bodies
// and hands decoded parts to the assembler for pwrite-based out-of-order
// assembly.
package app

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// ErrAlreadyStarted is returned by Start on the second call to a live
// Application.
var ErrAlreadyStarted = errors.New("app: already started")

const (
	defaultCheckpointInterval = constants.DefaultCheckpointInterval
	defaultCheckpointBytes    = constants.DefaultCheckpointBytes

	// closeHandlesTimeout bounds the pre-post-processing handle close. It
	// matches assembler.barrierOpTimeout, the bound on every other control
	// message that has to reach the single worker goroutine.
	closeHandlesTimeout = 5 * time.Second

	// shutdownCheckpointTimeout bounds R6's clean-shutdown barrier. It sits
	// inside the 15s per-step budget Shutdown gives the assembler stop that
	// follows it, so a wedged mount delays shutdown rather than preventing it.
	shutdownCheckpointTimeout = 10 * time.Second

	// reloadCheckpointTimeout is the BUDGET the barrier ReloadDownloader runs
	// before clearing the Emitted bits divides among resident jobs.
	//
	// It is deliberately not called a bound, because it does not bound the
	// call. checkpointJob acquires the per-job barrier lock before it looks
	// at any context, and sync.Mutex is not cancellable, so a job whose
	// barrier is already running under the periodic sweep is waited for
	// however long that sweep takes — a per-job hold of up to one full
	// checkpoint interval. The budget governs how the remaining time is
	// shared out, not when the call returns.
	//
	// A SEPARATE constant from shutdownCheckpointTimeout despite the equal
	// value, because the two bound different things and one is free to move.
	// Shutdown's sits inside a per-step budget it must not exceed; this one
	// paces a config change, and a user waiting on a settings save is a
	// different tolerance from a process on its way out.
	reloadCheckpointTimeout = 10 * time.Second
)

// Downloader defines the lifecycle and control interface for the Usenet
// article downloader.
type Downloader interface {
	Start(ctx context.Context) error
	Stop() error
	Completions() <-chan *downloader.ArticleResult
	SetSpeedLimit(bytesPerSec int64)
	SetDispatchOptions(maxArtTries, maxArtOpt int, topOnly bool, propagationDelay time.Duration)
	UnblockServer(name string) bool
	Pause()
	Resume()
	DisconnectAll()
}

// DownloaderStats defines the read-only observability interface for the downloader.
type DownloaderStats interface {
	Speed() float64
	SpeedLimit() int64
	ServerStatus() []downloader.ServerSnapshot
}

var (
	_ Downloader      = (*downloader.Downloader)(nil)
	_ DownloaderStats = (*downloader.Downloader)(nil)
)

// Application manages the download and post-processing pipeline.
type Application struct {
	version            string
	checkpointInterval time.Duration
	log                *slog.Logger

	// binaryVersions is populated once in New() from the startup probe
	// and never mutated afterward — safe to read from any goroutine
	// without synchronization (same pattern as the immutable version field).
	binaryVersions BinaryVersions
	mu             sync.Mutex
	// reloadMu serializes ReloadDownloader calls end-to-end. It is separate
	// from mu (which only guards the brief downloader/downloaderStats field
	// swap) so concurrent reloads queue up instead of interleaving their
	// Stop/setCompletions/checkpoint/ClearAllEmitted/Start sequences, which
	// would otherwise risk wiring app.downloader and app.pipeline's
	// completions source to two different downloader instances.
	reloadMu sync.Mutex
	config   *config.Config
	emitter  EventEmitter
	meter    *bpsmeter.Meter

	dispatcher      *dispatch.Dispatcher
	residency       *appResidency
	runner          *appRunner
	checkpointer    *checkpoint.Checkpointer
	historyRepo     *history.Repository
	downloader      Downloader
	downloaderStats DownloaderStats
	assembler       *assembler.Assembler
	// diskProbe bounds DownloadDirFreeBytes' statfs calls (from /health and
	// the status-overview API, both polled per-HTTP-request) to at most one
	// outstanding probe per directory — independent from assembler's own
	// diskProbe, since checkDiskSpace and DownloadDirFreeBytes are separate
	// call paths against the same directory. See assembler.DiskProbe.
	diskProbe        *assembler.DiskProbe
	postProcessor    *postproc.PostProcessor
	pipeline         *pipeline
	jobComplete      chan JobComplete
	postProcComplete chan PostProcComplete
	notifyDispatcher *notifier.Dispatcher

	internalFileComplete chan FileComplete
	onFileComplete       func(jobID string, fileIdx int)

	// barrier is the single place the Written -> Durable -> Resolved
	// transition happens (X2). It is the only thing that can mint a
	// DurableProof, and Job.AckDurable takes one, so no other path in the
	// program can ack an article as downloaded. nil when the process has no
	// history database, in which case nothing acks and every restart
	// re-downloads — see New.
	barrier *durability.Barrier
	// runs is the durability record itself. The barrier is its only writer;
	// everything here READS it — recordAssembledCRC for the whole-file CRC,
	// seedFromCommittedRuns for a stall recovery's replay — or deletes a
	// departed job's rows.
	runs    durability.RunStore
	resumer fileResumer

	// checkpointBytes is B1's volume bound. checkpointInterval above is its
	// time bound; the barrier fires on whichever arrives first.
	checkpointBytes int64

	// barrierKick carries an out-of-band checkpoint request from the write
	// path to the checkpoint loop, for a job that has crossed the byte bound.
	// Buffered and sent to non-blockingly: the write path must never wait on
	// the checkpoint loop.
	barrierKick chan string

	// barrierRuns counts completed Barrier.Run attempts. Observability, and
	// the only way a test can tell "the cadence fired" from "the cadence
	// happened to have nothing to do".
	barrierRuns atomic.Int64

	// barrierMu guards the two per-job maps below. It is NOT the barrier's
	// own lock: jobBarrierMu holds those, one per job, and they are held
	// across the barrier's I/O while this one never is.
	barrierMu       sync.Mutex
	jobBarrierMu    map[string]*barrierLock
	jobBarrierBytes map[string]int64
	// lastBarrier is when each job's last barrier completed without error.
	// R26 asks a job to be able to report it, and it is the figure that tells
	// "this job is checkpointing normally" from "this job has not had a
	// successful barrier since the mount went away".
	lastBarrier map[string]time.Time

	// stallMu guards stalls. It is never held across I/O: every walk copies
	// what it needs and releases first, because a re-evaluation runs barrier
	// operations against a mount that is suspected of being wedged.
	stallMu sync.Mutex
	stalls  map[string]*stallRecord

	// stallKick carries R19's "on user action" re-evaluation request from an
	// HTTP handler to the checkpoint loop. Buffered and sent to
	// non-blockingly, for the same reason barrierKick is.
	stallKick chan struct{}

	// stallRecheckInterval is R19's interval cadence. A field rather than the
	// constant directly so a test can drive the ticker arm of runCheckpoint's
	// select without waiting 30 seconds — the seam between the loop and
	// reevaluateStalls is otherwise unpinnable, and was.
	stallRecheckInterval time.Duration

	wg     sync.WaitGroup
	ctx    context.Context //nolint:containedctx // ctx is the app's lifecycle context, stored by design
	cancel context.CancelFunc

	started atomic.Bool
	stopped atomic.Bool

	// stopping is set at the top of Shutdown, BEFORE any of its steps run.
	//
	// It exists because the guard it feeds — Application.Stall's refusal to
	// park a job while the process is stopping — tested app.ctx.Err(), and
	// app.cancel() runs at step 4, AFTER the clean-shutdown checkpoint at step
	// 2. So the guard was inert on exactly the path it was written for: the
	// shutdown barrier exceeds its budget on a queue with many open files,
	// every job it reaches raises a fault, ctx.Err() is still nil, the pause
	// runs, and the queue.Save at the end of Shutdown persists it. The stall
	// list that would re-evaluate it is in-memory and dies with the process,
	// so the job comes back Paused forever.
	//
	// Reordering cancel() before the checkpoint is not the fix: it would
	// cancel the checkpoint's own context and stop it doing the work it exists
	// to do. Only a SIGTERM-cancelled parent context took the guarded path,
	// which is why this is a separate flag rather than a second look at ctx.
	stopping atomic.Bool

	// checkpointHook, when non-nil, runs at the top of shutdownCheckpoint.
	// Same discipline as the other same-package test seams: set once, before
	// the application is started. See shutdownCheckpoint for why the ordering
	// needs a seam at all.
	checkpointHook func()

	// jobCheckpointHook, when non-nil, runs with each job's checkpoint context
	// just before its barrier. Same discipline as checkpointHook.
	jobCheckpointHook func(context.Context)

	// postProcStopHook, when non-nil, overrides postProcessor.Stop during Shutdown.
	postProcStopHook func() error

	shutdownStepTimeout time.Duration
	closeHandlesTimeout time.Duration
	metricsPushInterval time.Duration

	bandwidthMax  atomic.Int64 // configured bandwidth ceiling in bytes/sec
	bandwidthPerc atomic.Int32 // configured bandwidth percentage (1-100)

	customStages []postproc.Stage
	stages       builtStages
	// probe holds the external-tool detection results captured once in New().
	// Immutable after construction. ReloadPostProcOptions reads it to carry the
	// construction-only option fields (unrar HasProblem, par2 Caps) forward
	// through the whole-struct stage Apply on reload. Zero value on the
	// customStages branch, where the concrete stages are nil and Apply is skipped.
	probe binaryProbe

	// duOrch owns the in-flight DirectUnpackers and their concurrency counter
	// under its own mutex (see directUnpackOrchestrator). Constructed in New().
	duOrch *directUnpackOrchestrator

	// finalizer handles the queue→history transition when a job finishes
	// post-processing (see jobFinalizer). Constructed in New().
	finalizer *jobFinalizer

	// lastHeartbeat stores the unix timestamp of the last active pipeline/download event.
	lastHeartbeat atomic.Int64
}

// SetNotifier injects a notification dispatcher for lifecycle events.
func (app *Application) SetNotifier(d *notifier.Dispatcher) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.notifyDispatcher = d
}

// New constructs an Application from cfg.
func New(cfg *config.Config, repo *history.Repository, opts ...func(*Application)) (*Application, error) {
	snap := cfg.Snapshot()
	gen := &snap.General
	dl := snap.Downloads
	dlDir := gen.DownloadDir
	completeDir := gen.CompleteDir
	adminDir := gen.AdminDir
	writeCacheBytes := int64(dl.WriteCacheSize)
	minFreeBytes := int64(dl.MinFreeSpace)
	maxActiveJobs := dl.MaxActiveJobs
	maxComputeSlots := dl.MaxComputeSlots
	sanitize := dl.SanitizeOptions()
	serversConfig := snap.Servers

	if dlDir == "" {
		return nil, errors.New("app: DownloadDir is required")
	}
	if completeDir == "" {
		return nil, errors.New("app: CompleteDir is required")
	}

	app := &Application{
		config:               cfg,
		historyRepo:          repo,
		emitter:              dummyEmitter{},
		internalFileComplete: make(chan FileComplete, 128),
		jobComplete:          make(chan JobComplete, 8),
		postProcComplete:     make(chan PostProcComplete, 8),
		ctx:                  context.Background(),
	}
	app.duOrch = newDirectUnpackOrchestrator(app)
	app.finalizer = newJobFinalizer(app)
	app.jobBarrierMu = make(map[string]*barrierLock)
	app.jobBarrierBytes = make(map[string]int64)
	app.lastBarrier = make(map[string]time.Time)
	app.stalls = make(map[string]*stallRecord)
	app.barrierKick = make(chan string, 64)
	app.stallKick = make(chan struct{}, 1)
	app.stallRecheckInterval = stallRecheckInterval
	app.checkpointInterval = time.Duration(dl.CheckpointInterval) * time.Second
	app.checkpointBytes = int64(dl.CheckpointBytes)
	app.shutdownStepTimeout = 15 * time.Second
	app.closeHandlesTimeout = closeHandlesTimeout
	app.metricsPushInterval = 1000 * time.Millisecond
	for _, o := range opts {
		o(app)
	}
	// Resolve both checkpoint bounds HERE, not in Start.
	//
	// They are read by noteJobBytes on the pipeline's worker goroutines and by
	// runCheckpoint on its own, and New is the last point at which nothing is
	// running yet — the options loop above is the final writer either way.
	// Resolving in Start would write the field after Start has already
	// launched pipeline.run, which is a data race with a concrete cost rather
	// than a theoretical one: this is where the DEFAULT is substituted, so a
	// stale read of the configured 0 (the documented "use the default") makes
	// `bytes >= checkpointBytes` true for every article and runs a full
	// barrier per article.
	app.checkpointInterval, app.checkpointBytes = checkpointSettings(app.checkpointInterval, app.checkpointBytes)
	if app.log == nil {
		app.log = slog.Default().With("component", "app")
	}
	log := app.log

	if app.meter == nil {
		app.meter = bpsmeter.NewMeter(10*time.Second, time.Now)
	}

	var dispatchStore dispatch.Store = nopDispatchStore{}
	var checkpointStore checkpoint.Store = &appCheckpointStore{}
	if repo != nil && repo.DB() != nil {
		dispatchStore = dispatchstore.New(repo.DB())
		checkpointStore = &appCheckpointStore{db: repo.DB()}
	}
	leaseCap := maxActiveJobs
	if leaseCap <= 0 {
		leaseCap = 4
	}
	slotCap := maxComputeSlots
	if slotCap <= 0 {
		slotCap = 2
	}
	mdir := manifestDir(adminDir)
	app.checkpointer = checkpoint.New(checkpointStore, 5*time.Second, log)
	var historyDB *sql.DB
	if repo != nil {
		historyDB = repo.DB()
	}
	app.residency = newAppResidency(app.lookupJob, mdir, historyDB, log)
	app.runner = newAppRunner(app)
	app.dispatcher = dispatch.New(
		leaseCap,
		slotCap,
		time.Second,
		time.Now,
		&appWorkers{app: app},
		app.residency,
		dispatchStore,
		app.runner,
	)
	app.runner.report = app.dispatcher

	// Probe sparse file support on the download directory. Pre-allocation
	// uses fallocate/ftruncate which benefits from sparse-capable filesystems.
	if supported, msg := assembler.CheckSparseSupport(dlDir); !supported {
		log.Warn(msg)
	} else {
		log.Info(msg)
	}

	if writeCacheBytes > 0 {
		log.Info("write coalescing enabled",
			"cacheMiB", writeCacheBytes/(1024*1024))
	}

	if app.downloader == nil {
		servers := make([]*downloader.Server, len(serversConfig))
		for i, sc := range serversConfig {
			servers[i] = downloader.NewServer(sc)
		}
		realDL := downloader.New(app.dispatcher, servers, app.meter, app.buildDownloaderOptions(), log)
		app.downloader = realDL
		app.downloaderStats = realDL
	}

	// Apply initial bandwidth limit from config.
	dl = app.config.GetDownloads()
	bandwidthMax := int64(dl.BandwidthMax)
	bandwidthPerc := int(dl.BandwidthPerc)

	app.bandwidthMax.Store(bandwidthMax)
	perc := bandwidthPerc
	if perc <= 0 || perc > 100 {
		perc = 100
	}
	app.bandwidthPerc.Store(int32(perc))
	if bandwidthMax > 0 {
		app.downloader.SetSpeedLimit(bandwidthMax * int64(perc) / 100)
	}

	p := &pipeline{
		log:         log.With("component", "pipeline"),
		dispatcher:  app.dispatcher,
		completions: app.downloader.Completions(),
		downloadDir: dlDir,
		sanitize:    sanitize,
		onHeartbeat: app.RecordHeartbeat,
		onJobHopeless: func(jobID string) {
			var msg string
			if app.dispatcher != nil {
				if j, ok := app.dispatcher.Job(jobID); ok {
					msg = failMsgForJob(j)
				}
			}
			if msg == "" {
				msg = "Aborted: 80%+ of first articles failed (DMCA'd or expired)"
			}
			app.maybeFinalize(jobID, msg)
		},
		onArticleWritten: app.noteJobBytes,
		checkpointer:     app.checkpointer,
		updateCh:         make(chan completionSwap, 1),
		fileInfo:         make(map[fileKey]assembler.FileInfo),
	}
	app.pipeline = p

	stageList := app.customStages
	if stageList == nil {
		probe := probeBinaries(app.ctx, cfg, log)
		app.binaryVersions = BinaryVersions{
			Par2Version:   probe.Par2Caps.Version,
			UnrarVersion:  probe.UnrarInfo.VersionStr,
			SevenzVersion: probe.SevenzInfo.Version,
		}
		built, err := buildStages(cfg, app.version, log, probe)
		if err != nil {
			return nil, err
		}
		stageList = built.Stages
		app.stages = built
		app.probe = probe
	}
	// else: app.customStages was supplied via WithPostProcStages, so
	// app.stages is left as the zero builtStages{} (all-nil pointers) —
	// behaviourally identical to the pre-refactor nil stage-pointer fields.
	pp := postproc.New(postproc.Options{
		Stages: stageList,
		OnOutput: func(jobID, tool, line string) {
			app.emit(Event{
				Type:  "postproc_output",
				NzoID: jobID,
				Tool:  tool,
				Line:  line,
			})
		},
		OnJobDone: app.finalizer.finalize,
		Logger:    log,
	})
	app.postProcessor = pp

	onFileComplete := func(jobID string, fileIdx int) {
		// No CRC32 is reported. The assembler cannot compute a whole-file
		// one honestly — a resumed run is never sent the articles an earlier
		// run completed, so its parts never tile the file (#349) — and the
		// honest figure is the crc32 of a file's single durable run, which
		// Application.recordAssembledCRC queries after the finalize. Nothing
		// consumes this callback's would-be figure; see the note on
		// FileComplete.
		fc := FileComplete{JobID: jobID, FileIdx: fileIdx}
		select {
		case app.internalFileComplete <- fc:
		default:
			// Channel full — spawn goroutine on app.wg to ensure delivery.
			// Ordering constraint: this is safe w.r.t. wg.Add-during-Wait only because OnFileComplete
			// runs on the assembler worker, which Shutdown joins at step 2 — before app.wg.Wait() at step 4.
			//
			// This arm is why the channel's capacity is NOT a bound on
			// anything. A completed file now keeps its assembler handle open
			// until the consumer finalizes it (see assembler.finalizeFile), so
			// an unbounded backlog here is an unbounded set of open handles.
			// The arm is still required: blocking the worker on a consumer
			// whose finalize path submits control messages back to that same
			// worker deadlocks both.
			app.wg.Go(func() {
				app.internalFileComplete <- fc
			})
		}
	}
	app.onFileComplete = onFileComplete

	// The durability record lives in the history database, which is also where
	// the queue's own state lives. One database, one set of transactions, no
	// second file to keep consistent with it.
	//
	// A process with no history database gets no barrier, and that is a
	// degraded mode rather than a supported one: the barrier is the only
	// thing that can mint a DurableProof, so nothing acks, and every restart
	// re-downloads everything. It is logged loudly for that reason. Only
	// tests that never download reach it.
	if repo != nil && repo.DB() != nil {
		app.runs = durability.NewSQLiteRunStore(repo.DB())
		app.resumer = durability.NewResumer(app.runs, log)
		app.barrier = durability.NewBarrier(app.runs, app, app, log)
	} else {
		log.Warn("no history database: durability barrier disabled, " +
			"no article will be acked and every restart re-downloads the whole queue")
	}

	// The assembler is given no SUCCESS ack callback. It has no authority to
	// resolve an article as downloaded any more: successes are acked by
	// durability.Barrier, which is the only component that runs the fsync
	// (X2). The barrier's cadence lives in runCheckpoint.
	//
	// OnArticleRejected is the one exception in the other direction, and it is
	// not the assembler resolving anything: it reports an article it refused,
	// and handleArticleRejected does the acking. The refusal has no other
	// witness — a bad yEnc offset is caught here and nowhere else — so without
	// this the article is silently dropped and its job never finishes. Ordinary
	// permanent failures still go to Job.MarkArticleFailed from the
	// pipeline.
	//
	// Options carries no durability store either, and nothing here records
	// anything about an article. The assembler REPORTS what its writes
	// reached — offset, length and CRC come back in the drain — and the
	// barrier records them after the fsync. A store wired in here would put a
	// writer on the assembler's worker goroutine, where a SQLite stall is read
	// as evidence about storage and parks a healthy job (A1).
	asm := assembler.New(assembler.Options{
		FileInfo:            p.resolveFileInfo,
		MinFreeBytes:        minFreeBytes,
		WriteCacheBytes:     writeCacheBytes,
		OnLowDisk:           app.handleLowDisk,
		OnWriteFault:        app.handleWriteFault,
		OnArticlesUnwritten: app.handleArticlesUnwritten,
		OnArticleRejected:   app.handleArticleRejected,
		OnPostAnomaly:       app.handlePostAnomaly,
		OnFileComplete:      onFileComplete,
	}, log)
	app.assembler = asm
	p.assembler = asm
	app.diskProbe = assembler.NewDiskProbe(assembler.DefaultDiskProbeTTL)
	app.RecordHeartbeat()

	return app, nil
}

// WithCheckpointInterval overrides the checkpoint cadence, for tests that
// cannot wait 30 seconds for one.
//
// It is one interval, not two, because there is only one thing to schedule:
// each tick runs a durability barrier per active job and then saves the queue.
// The save follows the barrier because the barrier is what produces something
// worth saving — an ack marks articles Done in memory, and until the queue is
// written a crash re-fetches them anyway.
func WithCheckpointInterval(d time.Duration) func(*Application) {
	return func(a *Application) { a.checkpointInterval = d }
}

// WithStallRecheckInterval overrides R19's re-evaluation cadence.
//
// Separate from the checkpoint interval because they measure different things:
// one bounds rework on a healthy job, the other bounds how long a user waits
// after clearing a full disk. A test that drove the re-evaluation off the
// checkpoint interval would be asserting against a coupling production does
// not have.
func WithStallRecheckInterval(d time.Duration) func(*Application) {
	return func(a *Application) { a.stallRecheckInterval = d }
}

// WithCheckpointBytes overrides B1's volume bound, for tests that cannot
// download 64 MiB to see one barrier.
func WithCheckpointBytes(n int64) func(*Application) {
	return func(a *Application) { a.checkpointBytes = n }
}

// WithShutdownStepTimeout overrides the per-step shutdown timeout for tests.
func WithShutdownStepTimeout(d time.Duration) func(*Application) {
	return func(a *Application) { a.shutdownStepTimeout = d }
}

// WithCloseHandlesTimeout overrides the handle close timeout before post-processing for tests.
func WithCloseHandlesTimeout(d time.Duration) func(*Application) {
	return func(a *Application) { a.closeHandlesTimeout = d }
}

// WithMetricsPushInterval overrides the interval at which metrics are pushed to the emitter.
func WithMetricsPushInterval(d time.Duration) func(*Application) {
	return func(a *Application) { a.metricsPushInterval = d }
}

func uniqueName(base string, exists func(string) bool) string {
	name := base
	for i := 1; exists(name); i++ {
		name = fmt.Sprintf("%s.%d", base, i)
	}
	return name
}

// Speed returns the current aggregate download speed in bytes/sec, or 0
// when downloading is idle or the downloader stats interface has not been wired yet.
func (app *Application) Speed() float64 {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloaderStats == nil {
		return 0
	}
	return app.downloaderStats.Speed()
}

// detectDuplicateNZB checks whether job's MD5 or filename already exists in
// the active queue, history DB, or the admin/nzb/ backup directory. Returns
// whether it's a duplicate and the Warning string AddJob should attach to
// the job (empty if not a duplicate). Split out of AddJob to isolate the
// duplicate-detection branching from queue insertion (OPT-9).
func (app *Application) detectDuplicateNZB(ctx context.Context, md5, filename string, force bool, nzbDir string) (isDuplicate bool, warning string) {
	dupReason := ""
	if app.dispatcher != nil && md5 != "" {
		for _, row := range app.dispatcher.List() {
			if row.Header.MD5 != "" && row.Header.MD5 == md5 {
				isDuplicate = true
				dupReason = "found in active queue (MD5)"
				break
			}
		}
	}
	if !isDuplicate && app.historyRepo != nil && md5 != "" {
		results, err := app.historyRepo.Search(ctx, history.SearchOptions{MD5Sum: md5})
		if err == nil && len(results) > 0 {
			isDuplicate = true
			dupReason = fmt.Sprintf("found in history DB (MD5: %q)", results[0].NzoID)
		}
	}
	if !isDuplicate && filename != "" {
		base := filepath.Base(filename)
		// Check for gzipped backup (current format) and uncompressed (legacy).
		if _, err := os.Stat(filepath.Join(nzbDir, base+".gz")); err == nil {
			isDuplicate = true
			dupReason = "found in admin/nzb/ backup dir (filename)"
		} else if _, err := os.Stat(filepath.Join(nzbDir, base)); err == nil {
			isDuplicate = true
			dupReason = "found in admin/nzb/ backup dir (filename, legacy)"
		}
	}
	if !isDuplicate {
		return false, ""
	}
	app.log.Info("duplicate NZB detected", "filename", filename, "md5", md5, "reason", dupReason, "forced", force)
	if !force {
		return true, "Duplicate NZB"
	}
	return true, "Duplicate NZB (Forced)"
}

// AddJob validates, deduplicates, and enqueues a new download job. If force
// is false and a duplicate is detected, the job is added in a paused state.
func (app *Application) AddJob(ctx context.Context, j *job.Job, hdr dispatch.Header, rawNZB []byte, force bool) error {
	adminDir := app.config.GetGeneral().AdminDir
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		return fmt.Errorf("app: mkdir admin nzb: %w", err)
	}

	isDuplicate, warning := app.detectDuplicateNZB(ctx, hdr.MD5, hdr.Filename, force, nzbDir)
	if isDuplicate {
		if !force {
			_ = j.SetIntent(job.IntentPause)
		}
		// Appended rather than assigned: BuildIngestJob may already have
		// recorded what the parser discarded, and a job can be both a
		// duplicate and malformed. Overwriting would drop the parse warning
		// silently, on exactly the jobs most likely to need it.
		if hdr.Warning != "" {
			hdr.Warning += "; " + warning
		} else {
			hdr.Warning = warning
		}
	}

	snap := app.config.Snapshot()
	gen := &snap.General
	downloadDir := gen.DownloadDir
	completeDir := gen.CompleteDir
	categories := snap.Categories
	hdr.Name = uniqueName(hdr.Name, func(name string) bool {
		if app.dispatcher != nil {
			for _, row := range app.dispatcher.List() {
				if row.Header.Name == name {
					return true
				}
			}
		}
		if _, err := os.Stat(filepath.Join(downloadDir, name)); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(completeDir, name)); err == nil {
			return true
		}
		for _, cat := range categories {
			if cat.Dir == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(completeDir, cat.Dir, name)); err == nil {
				return true
			}
		}
		return false
	})
	j.SetName(hdr.Name)

	if hdr.Filename != "" && len(rawNZB) > 0 {
		name, err := writeNZBBackup(nzbDir, hdr.Filename, rawNZB)
		if err != nil {
			app.log.Warn("failed to write gzipped NZB backup; job will not be retryable",
				"filename", hdr.Filename, "err", err)
		} else {
			hdr.NZBBackup = name
		}
	}

	mdir := manifestDir(adminDir)
	if err := os.MkdirAll(mdir, 0o750); err != nil {
		return fmt.Errorf("app: mkdir manifests: %w", err)
	}
	if m, err := j.Manifest(); err == nil && m != nil {
		data, mErr := json.Marshal(m)
		if mErr != nil {
			return fmt.Errorf("app: marshal manifest: %w", mErr)
		}
		mpath, pErr := manifestPath(adminDir, j.ID())
		if pErr != nil {
			return fmt.Errorf("app: write manifest: %w", pErr)
		}
		if err := fsutil.WriteGzAtomicBytes(mpath, data); err != nil {
			return fmt.Errorf("app: write manifest: %w", err)
		}
	}

	if app.dispatcher != nil {
		if err := app.dispatcher.Add(j, hdr); err != nil {
			return fmt.Errorf("app: add to dispatcher: %w", err)
		}
	}
	if app.historyRepo != nil && app.historyRepo.DB() != nil {
		if m, err := j.Manifest(); err == nil && m != nil {
			const qFiles = `
INSERT INTO job_files
  (job_id, file_index, subject, date, bytes, is_par2_recovery, complete, fetch_policy, filename, assembled_crc32, article_count, failed_bytes, bytes_downloaded)
VALUES (?, ?, ?, ?, ?, ?, 0, 0, '', 0, ?, 0, 0)
ON CONFLICT(job_id, file_index) DO NOTHING`
			for i := range m.NumFiles() {
				isPar2 := 0
				if m.FileIsPar2Recovery(i) {
					isPar2 = 1
				}
				lo, hi := m.FileRange(i)
				if _, err := app.historyRepo.DB().ExecContext(ctx, qFiles,
					j.ID(), i, m.FileSubject(i), m.FileDate(i).Unix(), m.FileBytes(i), isPar2, hi-lo,
				); err != nil {
					return fmt.Errorf("app: insert job_file %s index %d: %w", j.ID(), i, err)
				}
			}
		}
	}
	app.emit(Event{Type: "queue_updated"})
	app.log.Info("job added", "name", hdr.Name, "id", j.ID())
	return nil
}

// RemoveJob cancels and removes a job from the queue.
//
// deleteFiles says what happens to the bytes already on disk, and it governs
// every one of them: the whole-directory sweep at the end AND the assembler's
// per-file unlink of whatever it still holds open. Before #433 it gated only
// the sweep, so a caller asking to keep the job's data still lost the file
// that was mid-write — the one nothing else holds a copy of.
//
// What deleteFiles=false leaves behind is a partial: files preallocated to
// their expected size with holes where articles never arrived, in a directory
// nothing will reclaim, with no manifest or durable-run record left to
// interpret them (see deleteJobDurability below). That is the meaning of
// asking to keep a removed job's bytes — the caller wanted the data, not a
// resumable job.
func (app *Application) RemoveJob(ctx context.Context, id string, deleteFiles bool) error {
	if app.dispatcher == nil {
		return fmt.Errorf("job %q not found", id)
	}
	j, ok := app.dispatcher.Job(id)
	if !ok {
		return fmt.Errorf("job %q not found", id)
	}
	name := j.Name()

	// Abort any active DirectUnpacker for this job before removing files.
	app.duOrch.abortJob(id)

	// Cancel in-flight post-processing before any file is removed, so the PP
	// is not left operating on a directory that is being deleted.
	app.postProcessor.Cancel(id)

	_ = app.dispatcher.Cancel(id)
	if app.checkpointer != nil {
		app.checkpointer.Prune(id)
	}
	removeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := app.dispatcher.Remove(removeCtx, id); err != nil {
		return err
	}
	mpath, pErr := manifestPath(app.config.GetGeneral().AdminDir, id)
	if pErr != nil {
		return pErr
	}
	if rmErr := os.Remove(mpath); rmErr != nil && !os.IsNotExist(rmErr) {
		app.log.Debug("could not unlink manifest for removed job", "job", id, "err", rmErr)
	}

	disposition := assembler.KeepFiles
	if deleteFiles {
		disposition = assembler.DeleteFiles
	}
	if err := app.assembler.CancelJob(ctx, id, disposition); err != nil {
		app.log.Warn("assembler cancel job did not confirm file handles closed",
			"job", id, "error", err)
	}
	// Forget the pipeline's cached file info now that no more articles can
	// be dispatched for this job.
	app.pipeline.forgetJob(id)
	app.forgetJobBarrierState(id)
	// The job is gone from the queue, so nothing will ever read its durable
	// runs or its failed-article rows again. Both are keyed by job ID with no
	// foreign key to anything, so this is the only thing that removes them —
	// without it every deleted job leaves its rows behind for the life of
	// the database.
	app.deleteJobDurability(ctx, id)
	if deleteFiles && name != "" {
		downloadDir := app.config.GetGeneral().DownloadDir
		path := filepath.Join(downloadDir, name)
		if err := safeDeleteDir(path, downloadDir); err != nil {
			app.log.Warn("failed to delete job directory", "path", path, "err", err)
		}
	}
	app.emit(Event{Type: "queue_updated"})

	// Disconnect NNTP servers if no downloadable jobs remain.
	if !app.hasDownloadableJobs() {
		app.mu.Lock()
		dl := app.downloader
		app.mu.Unlock()
		// --- No lock held below this line ---
		if dl != nil {
			dl.DisconnectAll()
		}
	}
	return nil
}

func (app *Application) hasDownloadableJobs() bool {
	if app.dispatcher == nil || app.dispatcher.Paused() {
		return false
	}
	for _, row := range app.dispatcher.List() {
		if row.View.Reason.IsPause() {
			continue
		}
		if row.View.State == job.StateUnset || row.View.State == job.Fetching {
			return true
		}
	}
	return false
}

// RemoveHistoryJob deletes a completed job from history. If deleteFiles is true,
// the job's output directory is also removed.
func (app *Application) RemoveHistoryJob(ctx context.Context, id string, deleteFiles bool) error {
	if app.historyRepo == nil {
		return errors.New("history repository not wired")
	}
	entry, err := app.historyRepo.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("app: get history: %w", err)
	}
	if deleteFiles && entry.Path != "" {
		gen := app.config.GetGeneral()
		downloadDir := gen.DownloadDir
		completeDir := gen.CompleteDir
		// A history job's files may live under the complete dir (finished)
		// or the download dir (failed); allow either, refuse anything else.
		if err := safeDeleteDir(entry.Path, completeDir, downloadDir); err != nil {
			app.log.Warn("failed to delete history job directory", "path", entry.Path, "err", err)
		}
	}
	if _, err := app.deleteHistoryEntries(ctx, []history.Entry{*entry}); err != nil {
		return err
	}
	app.emit(Event{Type: "history_updated"})
	return nil
}

// deleteHistoryEntries removes history entries and the NZB backups they own.
//
// The retained per-file progress is cleaned up inside Repository.Delete, in
// the same transaction as the rows. The backups cannot be: they are files,
// and the history package has no business touching the admin directory. So
// this is the app-level choke point that every history deletion must route
// through — the SQL half closes by construction, this half by convention.
//
// A missing or unnamed backup is not an error. Entries written before the
// name was recorded have none, and the download itself never depended on it.
// Neither is it a reason to keep the entry: a stranded file must not outvote
// an operator asking for the row to go.
func (app *Application) deleteHistoryEntries(ctx context.Context, entries []history.Entry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	// Backups are unlinked before the rows go, not after.
	//
	// Neither order is atomic, so pick the failure that is visible. This
	// way a failed transaction leaves entries whose backup is missing, and
	// retrying one reports "open NZB backup" and says which file. The
	// reverse leaves files no row refers to — an unowned leak with nothing
	// to notice it, which is the shape #298 spent a whole PR removing.
	nzbDir := filepath.Join(app.config.GetGeneral().AdminDir, "nzb")
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.NZBBackup != "" {
			backupPath := filepath.Join(nzbDir, filepath.Base(entry.NZBBackup))
			if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
				app.log.Warn("failed to remove NZB backup for deleted history entry",
					"path", backupPath, "err", err)
			}
		}
		ids = append(ids, entry.NzoID)
	}
	return app.historyRepo.Delete(ctx, ids...)
}

// PruneHistory removes history entries past the configured retention
// thresholds, returning how many went.
//
// Both thresholds at zero — the default — makes this a no-op that does not
// touch the database. Selecting the entries first and then deleting them
// through deleteHistoryEntries is what keeps retention from orphaning the
// artifacts an entry owns (#303); the pruner it replaces issued its own
// DELETE and released neither.
func (app *Application) PruneHistory(ctx context.Context) (int, error) {
	if app.historyRepo == nil {
		return 0, nil
	}
	gen := app.config.GetGeneral()
	expired, err := app.historyRepo.ExpiredEntries(ctx, gen.HistoryRetentionDays, gen.HistoryFailedRetentionDays)
	if err != nil {
		return 0, fmt.Errorf("app: history retention: %w", err)
	}
	if len(expired) == 0 {
		return 0, nil
	}
	n, err := app.deleteHistoryEntries(ctx, expired)
	if err != nil {
		return 0, fmt.Errorf("app: history retention delete: %w", err)
	}
	app.log.Info("history retention removed expired entries",
		"count", n, "retain_days", gen.HistoryRetentionDays,
		"retain_failed_days", gen.HistoryFailedRetentionDays)
	app.emit(Event{Type: "history_updated"})
	return n, nil
}

// GetHistory retrieves a single history entry by ID.
func (app *Application) GetHistory(ctx context.Context, id string) (*history.Entry, error) {
	if app.historyRepo == nil {
		return nil, errors.New("history repository not wired")
	}
	return app.historyRepo.Get(ctx, id)
}

// JobComplete returns the channel signalled when all files in a job are done.
func (app *Application) JobComplete() <-chan JobComplete { return app.jobComplete }

// PostProcComplete returns the channel signalled when post-processing finishes.
func (app *Application) PostProcComplete() <-chan PostProcComplete { return app.postProcComplete }

// Start launches the download pipeline, assembler, and background goroutines.
// It blocks until all components are running. Call Shutdown to stop.
//
// Start is not retryable. On error the caller must surface it and exit; both
// production callers do (cmd/gonzbd/main.go, serve and one-shot modes), and a
// supervisor restarting the process gets a fresh Application anyway. Retrying
// in place would not work regardless: PostProcessor.Start has already flipped
// its own started flag by the time the later steps can fail, and it has no
// reset, so a second attempt fails with postproc.ErrAlreadyStarted.
//
// started is still reset to false on the failure path below, so the object is
// left in a clean rather than half-armed state — but that is hygiene, not a
// supported retry contract.
//
// Invariant: ReloadDownloader must not be called until Start has returned.
// started flips true (via CompareAndSwap) before this method finishes
// constructing the pipeline, and a ReloadDownloader call that raced in
// during that window could Stop the same downloader instance this method is
// concurrently Starting. This is safe today because the only caller
// (the config-reload HTTP handler) can't run until the API server starts
// listening, which happens after Start returns — see cmd/gonzbd/main.go.
func (app *Application) Start(ctx context.Context) error {
	if !app.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	// Leave the object in a clean rather than half-armed state on failure
	// (see the note on retryability above). The deferred function is disarmed
	// by setting succeeded=true before returning nil.
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		// Release the context created below. Shutdown cannot do it: its
		// started guard returns early once this defer clears the flag, so on
		// every error return nothing else would ever call cancel.
		//
		// This matters most for the late failure paths. By the time the
		// waitBounded steps can fail, four long-lived goroutines are already
		// running against app.ctx — pipeline.run, watchCompletions,
		// runCheckpoint and runMetricsPush. Without this cancel they would run
		// forever, and nothing would join them either, since app.wg.Wait lives
		// only in Shutdown.
		if app.dispatcher != nil {
			_ = app.dispatcher.Stop()
		}
		if app.cancel != nil {
			app.cancel()
		}
		app.started.Store(false)
	}()

	//nolint:gosec // G118: cancel is stored on the struct rather than a local,
	// so gosec cannot see the calls. It is invoked by Shutdown on the success
	// path and by the failure defer above on every error return.
	app.ctx, app.cancel = context.WithCancel(ctx)
	if app.dispatcher != nil {
		if err := app.dispatcher.Start(app.ctx); err != nil {
			return fmt.Errorf("app: start dispatcher: %w", err)
		}
	}
	if err := app.assembler.Start(app.ctx); err != nil {
		return err
	}
	// Clear the Emitted flags and un-fail the articles the old downloader's
	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if j, ok := app.dispatcher.Job(row.ID); ok {
				j.ClearEmittedForReload(false)
			}
		}
	}
	if err := app.resumeAllJobs(app.ctx); err != nil {
		_ = app.assembler.Stop()
		return err
	}
	// Snapshot app.downloader under app.mu once and reuse it below. started
	// flips true (via CompareAndSwap) before this point, so a concurrent
	// ReloadDownloader call could otherwise race an unguarded read of
	// app.downloader against its own field swap — the same torn-read class
	// fixed in #98. See handleLowDisk/Shutdown for the same pattern.
	app.mu.Lock()
	dl := app.downloader
	app.mu.Unlock()
	// --- No lock held below this line ---
	if err := dl.Start(app.ctx); err != nil {
		_ = app.assembler.Stop()
		return err
	}
	if err := app.postProcessor.Start(app.ctx); err != nil {
		_ = dl.Stop()
		_ = app.assembler.Stop()
		return err
	}
	app.pipeline.ctx = app.ctx // must be set before goroutine launch (setCompletions reads it)
	app.wg.Go(func() { app.pipeline.run(app.ctx) })
	app.wg.Go(func() { app.watchCompletions(app.ctx) })
	// Read only. Both bounds were resolved in New, before anything was
	// running; writing either here would race every goroutine launched above.
	interval := app.checkpointInterval
	app.wg.Go(func() { app.runCheckpoint(app.ctx, interval) })
	app.wg.Go(func() { app.runMetricsPush(app.ctx) })
	if app.checkpointer != nil {
		app.wg.Go(func() { _ = app.checkpointer.Run(app.ctx) })
	}

	app.log.Info("application started")

	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if app.historyRepo != nil && app.dropJobAlreadyInHistory(ctx, row.ID) {
				continue
			}
			if row.View.State == job.Fetching || row.View.State == job.StateUnset {
				j, ok := app.dispatcher.Job(row.ID)
				if ok && j.IsComplete() {
					failMsg := failMsgForJob(j)
					app.maybeFinalize(row.ID, failMsg)
				}
			}
		}
	}

	// Sweep expired history, after the reconciliation above and not before.
	//
	// That loop identifies a crash between the history commit and
	// Dispatcher.Remove by looking a still-queued completed job up in history,
	// and the entry is the only evidence the job already finished. Pruning
	// first can delete it out from under the lookup, which turns into
	// ErrNotFound and sends the job to maybeFinalize to be post-processed
	// and filed a second time. The trigger is ordinary: a crash, a daemon
	// down past the threshold, a restart.
	//
	// A startup sweep is needed at all because the other trigger is job
	// finalization, matching upstream (sabnzbd/postproc.py calls
	// auto_history_purge there) — so a daemon that finishes nothing never
	// prunes, and the instance that sat idle over the threshold would keep
	// its expired history forever. Deliberately not on the checkpoint
	// ticker: that fires every 30 seconds against a policy measured in days.
	if _, err := app.PruneHistory(app.ctx); err != nil {
		app.log.Warn("history retention sweep failed at startup", "err", err)
	}

	succeeded = true
	return nil
}

// waitBounded executes wait in a background goroutine and waits up to duration d for it to return.
// If wait completes within d, it returns wait's error.
// If duration d elapses first, it logs an error ("shutdown step exceeded budget; abandoning")
// and returns a step timeout error.
func waitBounded(name string, d time.Duration, wait func() error, log *slog.Logger) error { //nolint:unparam // d is configurable per step
	errCh := make(chan error, 1)
	go func() {
		errCh <- wait()
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		log.Error("shutdown step exceeded budget; abandoning", "step", name, "budget", d)
		return fmt.Errorf("step %s timed out after %v", name, d)
	}
}

// finalBarrier tells stopWorkers whether to run R6's clean-shutdown checkpoint
// between stopping the downloader and stopping the assembler.
//
// Shutdown passes barrierOnStop. ForceStopWorkers passes noBarrierOnStop
// because its whole purpose is to reproduce a hard kill, and a process that
// was SIGKILLed did not get to flush anything.
type finalBarrier bool

const (
	barrierOnStop   finalBarrier = true
	noBarrierOnStop finalBarrier = false
)

// stopWorkers stops the downloader, optionally runs the clean-shutdown barrier,
// aborts active DirectUnpackers, and stops the assembler, in exactly that order.
// Shared between Shutdown and ForceStopWorkers (in export_test.go) so the
// teardown ordering cannot drift between them.
//
// The barrier sits between the downloader stopping and the assembler stopping
// because that is the only window where both halves hold: no new article can
// arrive, and the file handles the barrier needs still exist. See
// Application.shutdownCheckpoint.
func (app *Application) stopWorkers(stepTimeout time.Duration, errs *[]error, barrier finalBarrier) {
	// Barrier on reloadMu: stopped is now true, so any ReloadDownloader call
	// that arrives after this point sees it and returns immediately without
	// doing any work. But a reload already past that check when we set
	// stopped could still be mid-flight, and would otherwise finish after we
	// tear down below — swapping in a new downloader nobody is left to stop.
	// Acquiring and releasing reloadMu here waits for any such in-flight
	// reload to finish before we snapshot app.downloader, so the snapshot
	// below always reflects the final, settled downloader.
	app.reloadMu.Lock()
	app.mu.Lock()
	dl := app.downloader
	app.mu.Unlock()
	app.reloadMu.Unlock()
	// --- No lock held below this line ---

	var dlErr error
	if dl != nil {
		if dlErr = waitBounded("downloader", stepTimeout, dl.Stop, app.log); dlErr != nil && errs != nil {
			*errs = append(*errs, fmt.Errorf("downloader stop: %w", dlErr))
		}
		// If dl.Stop returned cleanly with no error, all downloader workers have definitely
		// exited and will not touch manifests or barriers again. Yield Fetching jobs so
		// Dispatcher.Stop can cleanly park and evict. If dl.Stop timed out, do NOT yield,
		// so Dispatcher.Stop observes wait worker timeout and skips eviction.
		if dlErr == nil && app.dispatcher != nil {
			for _, row := range app.dispatcher.List() {
				if row.View.State == job.Fetching {
					_ = app.dispatcher.Yielded(row.ID)
				}
			}
		}
	}

	// R6's clean-shutdown barrier, in the only window where both halves
	// hold: the downloader has stopped so no new article can arrive, and the
	// assembler has not, so the file handles the barrier needs still exist.
	// Without it every byte since the last checkpoint is re-fetched on the
	// next start — a full window thrown away on a deliberate restart.
	if barrier {
		app.shutdownCheckpoint()
	}

	// Abort all active DirectUnpackers before stopping the assembler.
	// This kills unrar subprocesses and cleans up partial extracts.
	app.duOrch.abortAll()

	if app.assembler != nil {
		if err := waitBounded("assembler", stepTimeout, app.assembler.Stop, app.log); err != nil && errs != nil {
			*errs = append(*errs, fmt.Errorf("assembler stop: %w", err))
		}
	}
}

// Shutdown stops the downloader, post-processor, and assembler, flushes the
// cache, and persists the queue to disk. Safe to call multiple times.
//
// Ordering matters:
//  1. Stop the downloader — no new articles are dispatched.
//  2. Run R6's clean-shutdown barrier, so work since the last checkpoint is not
//     re-fetched on the next start, and abort active DirectUnpackers.
//  3. Stop the assembler — drains in-flight writes and delivers any remaining
//     OnFileComplete events to watchCompletions, which is still running.
//  4. Cancel the context — watchCompletions exits.
//  5. Wait for background goroutines to finish.
//  6. Stop the post-processor, save queue.
//
// Steps 1-3 are stopWorkers; see its doc for why the barrier sits between them.
func (app *Application) Shutdown() error {
	if !app.started.Load() || !app.stopped.CompareAndSwap(false, true) {
		return nil
	}
	// Before any step, so the stall guard covers the clean-shutdown barrier
	// below. See the field's doc.
	app.stopping.Store(true)

	// Pause the dispatcher queue immediately: sets q.paused, which blocks new
	// lease/slot grants by Advance and prevents job state moves during teardown,
	// eliminating state churn between worker yield and dispatcher stop.
	if app.dispatcher != nil {
		app.dispatcher.Pause()
	}

	var errs []error
	stepTimeout := app.shutdownStepTimeout
	if stepTimeout <= 0 {
		stepTimeout = 15 * time.Second
	}

	app.stopWorkers(stepTimeout, &errs, barrierOnStop)

	app.cancel()

	if err := waitBounded("wg.Wait", stepTimeout, func() error {
		app.wg.Wait()
		return nil
	}, app.log); err != nil {
		errs = append(errs, fmt.Errorf("wg wait: %w", err))
	}

	ppStopFn := app.postProcessor.Stop
	if app.postProcStopHook != nil {
		ppStopFn = app.postProcStopHook
	}
	ppErr := waitBounded("postprocessor", stepTimeout, ppStopFn, app.log)
	if ppErr != nil {
		errs = append(errs, fmt.Errorf("postprocessor stop: %w", ppErr))
	}
	if app.dispatcher != nil {
		if ppErr == nil {
			for _, row := range app.dispatcher.List() {
				if row.View.State == job.Repairing || row.View.State == job.Extracting || row.View.State == job.Finalizing {
					_ = app.dispatcher.Yielded(row.ID)
				}
			}
		}
	}

	if app.dispatcher != nil {
		if err := waitBounded("dispatcher", stepTimeout, app.dispatcher.Stop, app.log); err != nil {
			errs = append(errs, fmt.Errorf("dispatcher stop: %w", err))
		}
	}

	if app.checkpointer != nil {
		if err := app.checkpointer.Flush(context.Background()); err != nil {
			errs = append(errs, fmt.Errorf("checkpointer flush: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (app *Application) watchCompletions(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Drain any pending completions so they're applied to
			// the queue before it's saved to disk during shutdown.
			app.drainCompletions(ctx)
			return
		case fc, ok := <-app.internalFileComplete:
			// A closed channel is permanently ready, so without this guard the
			// select would spin at full CPU forever, handing zero-value events
			// to handleFileComplete. Nothing closes internalFileComplete today.
			// No drain is needed on !ok: a closed buffered channel yields every
			// buffered value before ok goes false.
			//
			// Logged at Error because exiting here is not a clean shutdown: the
			// completion consumer disappears while the app keeps running, so
			// jobs stall at 100% and post-processing never starts. Silence would
			// make that harder to diagnose than the spin it replaces.
			if !ok {
				app.log.Error("internalFileComplete was closed; completion consumer exiting, jobs will stall")
				return
			}
			app.handleFileComplete(ctx, fc)
		}
	}
}

// logQueueWriteFailure reports a failed per-file queue write at a level that
// matches what the failure means.
//
// A job that was removed, or whose manifest was evicted, between the
// assembler producing this event and the queue receiving it is ordinary — the
// queue methods report it as dispatch.ErrNotFound/job.ErrNotResident precisely so a
// caller can recognise it rather than having it arrive as a silent nil (#261).
// Those get Debug. Anything else is a write that should have landed and did
// not, and gets Warn.
func (app *Application) logQueueWriteFailure(op, jobID string, fileIdx int, err error) {
	if errors.Is(err, job.ErrNotResident) || errors.Is(err, dispatch.ErrNotFound) {
		app.log.Debug(op+" skipped, job no longer resident in the queue",
			"job", jobID, "fileidx", fileIdx, "err", err)
		return
	}
	app.log.Warn(op+" failed", "job", jobID, "fileidx", fileIdx, "err", err)
}

// handleFileComplete processes a single file completion event.
func (app *Application) handleFileComplete(ctx context.Context, fc FileComplete) {
	// FIRST, before anything downstream can act on the file. finalizeCompletedFile
	// runs the barrier over it, trims it to its real extent and hands the
	// handle back to the assembler. Everything below — MarkFileComplete,
	// DirectUnpack, job finalization and the post-processing that follows —
	// reads or consumes those bytes, and a file that still carries
	// pre-allocation's trailing zeros is one par2 reports as damaged.
	//
	// A failure here therefore STOPS the completion rather than being logged
	// past. The file is not marked complete, DirectUnpack is not fed it, and
	// the job does not finalize — because none of those can be undone once
	// done, while a stalled job can be resumed by an operator who has fixed
	// the mount. The job pauses with the reason attached (A1, R19, R27): a
	// failure to trim is a condition of storage, so no article is marked
	// failed, the failed-byte count and the health percentage are untouched,
	// and every article stays exactly as durable as it already was.
	//
	// The path is resolved BEFORE the finalize, not after. Application.Fail
	// carries a permanently faulted job into maybeFinalize, which drops the
	// pipeline's cached FileInfo for it — so a path asked for afterwards comes
	// back empty and the reason the operator is shown names no file at all.
	// That is a bug on its own, independent of the double-routing below.
	path := app.filePathFor(fc.JobID, fc.FileIdx)
	if err := app.finalizeCompletedFile(ctx, fc.JobID, fc.FileIdx); err != nil {
		app.routeFinalizeFailure(fc.JobID, fc.FileIdx, path, err)
		return
	}
	if err := app.completeFinalizedFile(ctx, fc); err != nil {
		// Recorded, not just logged. The barrier has already trimmed this
		// file, acked its articles and released the handle, and the
		// assembler's tombstone means OnFileComplete will never fire for it
		// again — so nothing else IN THIS PROCESS can re-trigger this. Dropping
		// it leaves the file's Complete flag false with every article Done and
		// nothing left to dispatch: a wedged job.
		//
		// It no longer survives restarts — completeStrandedFiles finishes the
		// interrupted finalize during the next start's resume sweep — and this
		// note stays because a restart is not an acceptable recovery for a
		// condition the running process can fix itself. The retry is what keeps
		// the repair in-process; the sweep is the backstop for the crash that
		// takes the note down with it.
		app.log.Info("completion not delivered; recorded for the stall re-evaluation to retry",
			"job", fc.JobID, "fileidx", fc.FileIdx, "err", err)
		app.noteUndeliveredCompletion(fc.JobID, fc.FileIdx)
	}
}

// completeFinalizedFile is everything the completion path does AFTER the
// file's bytes on disk are known to be correct.
//
// Split out of handleFileComplete because a stall raised by a failed finalize
// interrupts the completion between the two halves, and
// Application.reevaluateStall has to resume it from exactly there — the
// finalize retried on its own, then this. Inlining it would have meant a
// second copy of the mark-complete/DirectUnpack/finalize sequence, free to
// drift from this one (S5).
//
// It returns an error only so the retry can tell whether the queue accepted
// the completion: MarkFileComplete needs the job resident, and a job the
// active set had no room to re-promote must be tried again rather than have
// its completion silently dropped. handleFileComplete's own call has already
// logged everything a live pipeline can act on.
func (app *Application) completeFinalizedFile(ctx context.Context, fc FileComplete) error {
	// The assembled CRC is recorded HERE rather than beside the barrier,
	// because this function is what every completion path shares and the CRC
	// is part of "everything the completion path does after the bytes are
	// correct". Beside the barrier it was reached by ONE of the three:
	//
	//   - the ordinary path reached it, since finalizeCompletedFile returns
	//     nil and its caller comes straight here;
	//   - stall recovery did not. retryFinalize swallows job.ErrNotResident —
	//     the ack cannot mark a paused job — and returns before the CRC, then
	//     Phase 4 arrives here having marked the file finalizeDone, and
	//     nothing retried it;
	//   - the startup repair did not, because completeStrandedFiles has no
	//     barrier call to hang it off at all.
	//
	// Both of those left AssembledCRC32 at zero, which par2 reads as NoCRC:
	// the QuickCheck bypass is suppressed and every recovery volume is fetched
	// for a file whose whole-file CRC was sitting in its durable run the
	// entire time. One owner rather than three call sites is what stops a
	// fourth path from being added without one.
	//
	// Before MarkFileComplete, so the value is on the progress record by the
	// time maybeFinalize below can hand the job to post-processing.
	app.recordAssembledCRC(ctx, fc.JobID, fc.FileIdx)
	if app.dispatcher != nil {
		j, ok := app.dispatcher.Job(fc.JobID)
		if !ok {
			err := job.ErrNotResident
			app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
			return err
		}
		if err := j.MarkFileComplete(fc.FileIdx); err != nil {
			app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
			return err
		}
		if app.checkpointer != nil {
			app.checkpointer.Mark(j)
		}
		if j.IsComplete() {
			_ = j.SetNext(job.Assessing)
			if err := app.dispatcher.Yielded(fc.JobID); err != nil {
				app.dispatcher.Wake()
			}
		}
	}
	app.emit(Event{Type: "queue_updated"})

	// DirectUnpack: feed completed RAR volumes to the unpacker for
	// streaming extraction during download.
	pp := app.config.GetPostProc()
	directUnpack := pp.DirectUnpack
	enableUnrar := pp.EnableUnrar
	if directUnpack && enableUnrar {
		app.duOrch.maybeStart(fc)
	}

	return nil
}

// maybeReleaseRecoveryVolumes checks whether a completed job with deferred par2
// recovery volumes needs repair. If so it un-defers the volumes, broadcasts a
// queue update, and returns true — the caller must not finalize yet (the
// downloader will fetch the volumes and trigger another completion event).
//
// Returns false when: there are no deferred volumes, the data verifies clean,
// the verdict is unknown (nothing on disk could be identified against the
// par2 index, so the volumes are held rather than spent or discarded), or
// un-deferral itself fails (in which case we fall through to finalize without
// recovery volumes, matching the pre-on-demand-par2 behaviour).
func (app *Application) maybeReleaseRecoveryVolumes(ctx context.Context, jobID string) bool {
	if ctx.Err() != nil {
		return false
	}
	if app.dispatcher == nil {
		return false
	}
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		return false
	}

	if !j.HasDeferredPar2() {
		return false
	}
	if p := j.Progress(); p != nil && p.Par2Recovered() {
		return false
	}
	jobName := j.Name()

	cfgSnap := app.config.Snapshot()
	downloadDir := cfgSnap.General.DownloadDir
	pp := &cfgSnap.PostProc
	parseOpts := par2.ParseOptionsFromConfig(pp)
	dir := filepath.Join(downloadDir, jobName)

	outcome, reason := outcomeRepair, "manifest unreadable, cannot verify integrity"
	m, err := j.Manifest()
	if err != nil {
		app.log.Warn("on-demand par2: cannot verify without the manifest; post-processing will run a full par2 verify instead",
			"job", jobID, "err", err)
	} else if prog := j.Progress(); prog != nil {
		sets, sErr := par2.FindPar2Files(dir, parseOpts)
		switch {
		case sErr != nil || len(sets) == 0:
			outcome = outcomeRepair
			reason = "no usable par2 index found to verify against"
			if sErr != nil {
				reason = fmt.Sprintf("no usable par2 index found (err: %v)", sErr)
			}
			app.log.Info("on-demand par2: no usable par2 index to verify against; fetching recovery volumes",
				"job", jobID, "dir", dir, "err", sErr)
		default:
			a, aErr := par2.AssessWithOptions(dir, sets, assembledFiles(m, prog), app.log, parseOpts)
			if aErr != nil {
				outcome = outcomeRepair
				reason = fmt.Sprintf("could not match downloaded files against the par2 index (err: %v)", aErr)
				app.log.Warn("on-demand par2: could not assess the download against the par2 index; fetching recovery volumes",
					"job", jobID, "dir", dir, "err", aErr)
				break
			}
			outcome, reason = par2Verdict(a, app.log)
		}
	}
	switch outcome {
	case outcomeClean:
		app.log.Info("on-demand par2: verified clean, skipping recovery volumes", "job", jobID)
		_ = j.DiscardDeferredPar2()
		return false
	case outcomeUnknown:
		j.SetPar2ReleaseReason(reason)
		app.log.Info("on-demand par2: nothing on disk matched the par2 index; holding the volumes and finalizing",
			"job", jobID, "reason", reason)
		return false
	case outcomeRepair:
		j.SetPar2ReleaseReason(reason)
		idxs := j.DeferredRecoveryIndices()
		if err := j.UndeferRecoveryVolumes(idxs); err != nil {
			app.log.Warn("on-demand par2: un-defer failed; finalizing without recovery volumes",
				"job", jobID, "err", err)
			failReason := fmt.Sprintf("%s; could not fetch recovery volumes: %v", reason, err)
			j.SetPar2ReleaseReason(failReason)
			return false
		}
		app.log.Info("on-demand par2: repair needed, fetching recovery volumes",
			"job", jobID, "volumes", len(idxs), "reason", reason)
		app.emit(Event{Type: "queue_updated"})
		return true
	default:
		app.log.Error("on-demand par2: unrecognized outcome; finalizing without acting on recovery volumes",
			"job", jobID, "outcome", outcome)
		return false
	}
}

// par2Outcome is what par2Verdict was able to determine about a completed
// job's deferred par2 recovery volumes.
//
// It replaces a two-valued needsRecovery bool, which could not express its
// third state and so collapsed two unrelated cases into one: "verified
// clean" and "nothing could be identified" both read as needsRecovery ==
// false, and the caller could not tell them apart. See outcomeUnknown for
// what that collapse cost.
type par2Outcome int

const (
	// outcomeClean means every par2-tracked file was identified and its
	// assembled CRC matched. The recovery volumes are spent for nothing and
	// may be discarded.
	outcomeClean par2Outcome = iota

	// outcomeRepair means at least one par2-tracked file is corrupt, has no
	// assembled CRC to check, could not be verified, or a par2 entry matched
	// no delivered file while others in the same set did match — repair is
	// possible and the volumes must be fetched.
	outcomeRepair

	// outcomeUnknown means nothing delivered matched any par2 entry, by name
	// or by content. That is a Layout B post — par2 protecting the extracted
	// contents rather than the delivered archives — and it is ALSO an
	// obfuscated single-file post damaged inside its first 16 KB, which
	// defeats identification passes 1, 2 and 3 together. The two are
	// indistinguishable from this value, so it must not be reported as a
	// clean verdict.
	//
	// Holding the volumes does not rescue the damaged case: nothing promotes
	// a held volume after the job finalizes, and ResetForRetry downgrades
	// FetchNever to FetchIfNeeded anyway, so a retry behaves the same under
	// either policy. What the hold buys is an honest label — fileState
	// renders FetchIfNeeded as "held" and FetchNever as "skipped"
	// (internal/api/queue.go), and "skipped" would assert a verdict that was
	// never earned.
	outcomeUnknown
)

// allPar2Outcomes returns every declared outcome, so a test can assert that
// a switch over them handles each one rather than falling through silently.
// Kept in declaration order.
//
// It is hand-written, which on its own would make it a second copy of the
// enum carrying the same defect: a value added to the const block but not
// here is invisible to every loop over it, and every exhaustiveness test
// built on it passes vacuously. TestAllPar2Outcomes_Exhaustive closes that
// loop by parsing the const block itself, the same way
// postproc.AllQuickCheckOutcomes is pinned.
func allPar2Outcomes() []par2Outcome {
	return []par2Outcome{
		outcomeClean,
		outcomeRepair,
		outcomeUnknown,
	}
}

// String makes the outcome legible in logs and test failures rather than
// printing a bare integer.
func (o par2Outcome) String() string {
	switch o {
	case outcomeClean:
		return "clean"
	case outcomeRepair:
		return "repair"
	case outcomeUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// par2Verdict decides whether a completed job needs its deferred par2
// recovery volumes, from an assessment already taken.
//
// It performs no I/O and reads no queue state, and that is the point of its
// shape. Deciding "does this need repair" and moving files to their par2 names
// used to share one call chain, and the ORDER between them was #492 and #494:
// the move invalidated the names the decision matched on. par2.Assess answers
// everything about the filesystem in one pass from pre-rename state, and this
// function only reads that answer, so there is no ordering left to get wrong.
//
// Repair is needed when any par2-tracked file is corrupt (Mismatched), has no
// recorded CRC to check against (NoCRC), or could not be checked at all
// (Unverified). A job with no usable par2 index never reaches here — the
// caller fetches the volumes instead, which is the safe fallback.
func par2Verdict(a par2.Assessment, log *slog.Logger) (outcome par2Outcome, reason string) {
	id, r := a.ID, a.CRC

	// Accounting comes before verification, and it is a separate question.
	//
	// Identify asks which par2 entry each delivered file IS, by content where
	// the name does not say. An entry that matched nothing delivered means we
	// cannot rule out repair — whether because a file is missing or because a
	// name defeated us — and that wants the recovery volumes fetched.
	//
	// This replaces a guard that read "Matched == 0 && Mismatched == 0 &&
	// NoCRC == 0" as proof the par2 set described other files, and discarded
	// the volumes on it. That condition proved nothing: with obfuscated names
	// no delivered file matched a par2 entry by name, so all three counters
	// were zero whether the payload was pristine or shredded — the CRC was
	// never compared to anything. A real release reaching it was measured
	// (#492).
	if !id.Accounted() && len(id.Files) == 0 {
		// Nothing delivered matched ANY entry, by name or by content. That is
		// the Layout B signature — par2 protecting the EXTRACTED contents
		// rather than the delivered archives, so every entry names a file
		// that will not exist until unpack has run — and it is ALSO an
		// obfuscated single-file post damaged inside its first 16 KB, which
		// defeats identification passes 1, 2 and 3 together. The two are
		// indistinguishable from this value; see outcomeUnknown.
		//
		// The volumes are held (SetPar2ReleaseReason without discarding or
		// un-deferring, at the call site) rather than spent or dropped: for
		// Layout B they cannot be spent usefully — the branch that makes that
		// true is the STAGE ORDER, not anything about par2 (RepairStage runs
		// before UnpackStage, internal/app/stages.go, with no second repair
		// pass) — and for the damaged case, holding does not rescue it either.
		// What holding buys is that the fetch policy stays FetchIfNeeded
		// ("held") instead of being marked FetchNever ("skipped"), so the
		// on-disk state does not assert a verdict that was never earned.
		//
		// Distinguishing this from a partial shortfall is safe ONLY because
		// identification is content-based. Under name-only matching, "nothing
		// matched" was equally what a perfectly healthy obfuscated release
		// looked like — which is exactly what made discarding on this
		// signature the #492 defect. For a healthy obfuscated post, Hash16k
		// would tell them apart, but damage inside the first 16 KB defeats
		// Hash16k and the whole-file CRC32 check together, making it
		// indistinguishable from Layout B; that is why the outcome is
		// outcomeUnknown rather than clean or repair. This test may not be
		// reintroduced anywhere that matches on names.
		log.Info("on-demand par2: no delivered file matches any par2 entry; identification is inconclusive",
			"entries", len(id.Unaccounted))
		return outcomeUnknown, fmt.Sprintf("no delivered file matched any of %d par2-protected entr(y/ies); "+
			"could be a Layout B post (par2 protects extracted contents) or damage inside the first 16 KB of an obfuscated file",
			len(id.Unaccounted))
	}
	if !id.Accounted() {
		names := make([]string, 0, len(id.Unaccounted))
		for _, fd := range id.Unaccounted {
			names = append(names, fd.FileName)
		}
		log.Info("on-demand par2: par2 entries matched no delivered file; fetching recovery volumes",
			"unaccounted", len(id.Unaccounted))
		reason := fmt.Sprintf("%d par2-protected file(s) not found in this download (%s)",
			len(id.Unaccounted), strings.Join(names, ", "))
		// The files that WERE identified may still carry their own CRC
		// findings; fold those in rather than discarding them, using the
		// same summary construction crcVerdictParts builds for the
		// all-accounted case below.
		if crcParts := crcVerdictParts(r); len(crcParts) > 0 {
			reason += "; also " + strings.Join(crcParts, "; ")
		}
		return outcomeRepair, reason
	}

	if r.Mismatched+r.NoCRC+r.Unverified == 0 {
		return outcomeClean, ""
	}

	return outcomeRepair, strings.Join(crcVerdictParts(r), "; ")
}

// crcVerdictParts renders par2.Assessment.CRC's per-category findings
// (mismatched, missing-CRC, unverified) as human-readable clauses, one per
// non-zero category, in Mismatched/NoCRC/Unverified order. It is the sole
// place this rendering happens — par2Verdict's !id.Accounted() branch and
// its all-accounted branch both call it, rather than each building its own
// copy: `git grep -n 'crcVerdictParts(' internal/app/app.go` finds 4 lines —
// this comment's own self-match, the declaration below, and the two call
// sites in par2Verdict above.
func crcVerdictParts(r par2.CRCVerifyResult) []string {
	var parts []string
	if r.Mismatched > 0 {
		var corruptFiles []string
		for _, f := range r.Files {
			if !f.Match {
				corruptFiles = append(corruptFiles, f.FileName)
			}
		}
		parts = append(parts, fmt.Sprintf("corruption/CRC mismatch in %d file(s) (%s)",
			r.Mismatched, strings.Join(corruptFiles, ", ")))
	}
	if r.NoCRC > 0 {
		parts = append(parts, fmt.Sprintf("failed download in %d file(s) (%s)",
			r.NoCRC, strings.Join(r.NoCRCFiles, ", ")))
	}
	if r.Unverified > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) unverified", r.Unverified))
	}
	return parts
}

// drainCompletions processes all buffered events on internalFileComplete.
func (app *Application) drainCompletions(ctx context.Context) {
	for {
		select {
		case fc, ok := <-app.internalFileComplete:
			// Without this guard a closed channel would keep this receive
			// permanently ready, so `default` would never be selected and the
			// drain would spin forever on zero-value events instead of
			// returning. Nothing closes internalFileComplete today; if that
			// changes, say so rather than exiting quietly.
			if !ok {
				app.log.Error("internalFileComplete was closed during drain; completions may be lost")
				return
			}
			app.handleFileComplete(ctx, fc)
		default:
			return
		}
	}
}

// hasFailedArticle reports whether any article in jf permanently failed
// (exhausted retries on all servers). Such a file can still reach the
// "complete" state — the assembler fires OnFileComplete once every article
// is Done-or-Failed, leaving gaps in the written file rather than blocking
// the job forever on data that will never arrive.
func hasFailedArticle(m *job.Manifest, p *job.JobProgress, fileIdx int) bool {
	lo, hi := m.FileRange(fileIdx)
	for i := lo; i < hi; i++ {
		if p.ArticleFailed(i) {
			return true
		}
	}
	return false
}

// DirectUnpackStatus returns the status of the direct unpacker for the given
// job. Delegates to the DirectUnpack orchestrator.
func (app *Application) DirectUnpackStatus(jobID string) (directunpack.Status, bool) {
	return app.duOrch.status(jobID)
}

// DirectUnpackStatuses returns a snapshot of every active direct-unpacker's
// status, keyed by job ID. Delegates to the DirectUnpack orchestrator, which
// takes its own private lock once regardless of job count — used by queueList
// to avoid re-locking per job in the listing hot path (OPT-12).
func (app *Application) DirectUnpackStatuses() map[string]directunpack.Status {
	return app.duOrch.statuses()
}

func (app *Application) maybeFinalize(jobID, failMsg string) { //nocover: defensive error logging on state transition
	if app.dispatcher == nil {
		return
	}
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		return
	}
	row, ok := app.dispatcher.Row(jobID)
	if !ok {
		return
	}
	app.enqueuePostProc(j, row.Header, failMsg)
}

// directUnpackWaiter is the subset of *directunpack.DirectUnpacker that
// awaitDirectUnpackOrAbort needs. Defined here (consumer side) so the wait
// logic can be unit-tested with a fake.
type directUnpackWaiter interface {
	Wait()
	Abort()
}

// awaitDirectUnpackOrAbort blocks until du finishes or ctx is cancelled. On
// natural completion it returns true. On cancellation it calls du.Abort() —
// which makes du.Wait() return — waits for the wait goroutine to exit, and
// returns false so the caller can skip post-processing during shutdown.
//
// This exists because a du handed to the async completion goroutine has already
// been removed from the orchestrator's unpackers map (via duOrch.collect), so
// Shutdown()'s abortAll cannot reach it; without this, a du.Wait() that blocks
// forever would hang app.wg.Wait() during shutdown.
func awaitDirectUnpackOrAbort(ctx context.Context, du directUnpackWaiter) bool {
	waited := make(chan struct{})
	go func() {
		du.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		return true
	case <-ctx.Done():
		du.Abort()
		<-waited
		return false
	}
}

func (app *Application) enqueuePostProc(j *job.Job, hdr dispatch.Header, failMsg string) {
	// Close any open assembler file handles for this job so post-processing
	// operations (Par2 repair, unpack, cleanup) don't trigger NFS silly-rename
	// (.nfsXXXX) artifacts on open files.
	closeTimeout := app.closeHandlesTimeout
	if closeTimeout <= 0 {
		closeTimeout = closeHandlesTimeout
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
	if err := app.assembler.CloseJobHandles(closeCtx, j.ID()); err != nil {
		app.log.Warn("enqueuePostProc: failed to close assembler job handles", "job", j.ID(), "err", err)
	}
	closeCancel()
	if app.checkpointer != nil {
		if err := app.checkpointer.Flush(context.Background()); err != nil {
			app.log.Warn("forced checkpoint flush on job completion failed", "job", j.ID(), "err", err)
		}
	}

	// Release cached file info for this job; the assembler no longer
	// needs it, and keeping it around leaks memory across many downloads.
	app.pipeline.forgetJob(j.ID())
	app.forgetJobBarrierState(j.ID())

	snap := app.config.Snapshot()
	gen := &snap.General
	downloadDirBase := gen.DownloadDir
	completeDir := gen.CompleteDir
	categories := snap.Categories
	sanitize := snap.Downloads.SanitizeOptions()
	downloadDir := filepath.Join(downloadDirBase, j.Name())

	// Log the handoff from download → postproc. This is the "entering
	// postproc" bookend; processJob logs the "exiting" bookend.
	var dlDuration time.Duration
	var failedBytes int64
	if prog := j.Progress(); prog != nil {
		if !prog.DownloadStarted().IsZero() && !prog.DownloadFinished().IsZero() {
			dlDuration = prog.DownloadFinished().Sub(prog.DownloadStarted())
		}
		failedBytes = prog.FailedBytes()
	}
	app.log.Info("postproc: job entering pipeline",
		"job", j.ID(),
		"name", j.Name(),
		"category", hdr.Category,
		"download_dir", downloadDir,
		"download_duration", dlDuration.Round(time.Second),
		"total_bytes", j.TotalBytes(),
		"failed_bytes", failedBytes,
		"fail_msg", failMsg,
	)

	// Log all files in the download directory so the history record
	// captures the exact starting state before any postproc stages.
	entries, err := os.ReadDir(downloadDir)
	if err == nil {
		for _, e := range entries {
			info, _ := e.Info()
			var sz int64
			if info != nil {
				sz = info.Size()
			}
			app.log.Info("postproc: download file",
				"job", j.ID(),
				"file", e.Name(),
				"size", sz,
				"dir", e.IsDir(),
			)
		}
	}

	cat := config.FindCategory(categories, hdr.Category)
	catDir := cat.Dir
	// P6: Category dir trailing '*' suppresses the per-job subfolder.
	// Files go directly into the category directory ("flat layout").
	// e.g. catDir="movies*" → complete_dir/movies/file.mkv
	//      catDir="movies"  → complete_dir/movies/JobName/file.mkv
	flatLayout := strings.HasSuffix(catDir, "*")
	if flatLayout {
		catDir = strings.TrimSuffix(catDir, "*")
	}
	finalDir := filepath.Join(completeDir, catDir, j.Name())
	if flatLayout {
		finalDir = filepath.Join(completeDir, catDir)
	}
	// Collect DirectUnpack results (if any) before enqueuing post-processing.
	// When du != nil, Wait() is executed inside an asynchronous worker
	// goroutine on app.wg so the completion event consumer (watchCompletions)
	// never blocks waiting for disk unpacking to finish.
	du := app.duOrch.collect(j.ID())

	enqueue := func(duResults map[string]directunpack.SuccessSet, duFailures map[string]directunpack.FailedSet, duSkipped map[string]directunpack.SkippedSet) {
		app.postProcessor.Process(&postproc.Job{
			Job:                  j,
			Filename:             hdr.Filename,
			NZBBackup:            hdr.NZBBackup,
			Category:             hdr.Category,
			Password:             hdr.Password,
			Script:               hdr.Script,
			PP:                   hdr.PP,
			URL:                  hdr.URL,
			DownloadDir:          downloadDir,
			FinalDir:             finalDir,
			Sanitize:             sanitize,
			FailMsg:              failMsg,
			DirectUnpackSets:     duResults,
			DirectUnpackFailures: duFailures,
			DirectUnpackSkipped:  duSkipped,
		})
		select {
		case app.jobComplete <- JobComplete{JobID: j.ID()}:
		default:
		}
	}

	if du != nil {
		app.wg.Go(func() {
			// du has already been removed from the orchestrator's unpackers map
			// above (via duOrch.collect), so Shutdown()'s abortAll can no longer
			// reach it. du.Wait() can
			// block indefinitely (e.g. waiting on a RAR volume that never
			// arrives), which would hang Shutdown() at app.wg.Wait(). Watch the
			// lifecycle context and Abort() the du on cancellation so Wait()
			// returns; skip dispatch since we are tearing down.
			if !awaitDirectUnpackOrAbort(app.ctx, du) {
				return
			}
			duResults := du.Results()
			duFailures := du.Failures()
			duSkipped := du.Skipped()
			if len(duResults) > 0 {
				app.log.Info("directunpack: passing results to postproc",
					"job", j.ID(), "sets", len(duResults))
			}
			if len(duFailures) > 0 {
				app.log.Warn("directunpack: passing failures to postproc",
					"job", j.ID(), "failed_sets", len(duFailures))
			}
			if len(duSkipped) > 0 {
				app.log.Info("directunpack: passing skipped sets to postproc",
					"job", j.ID(), "skipped_sets", len(duSkipped))
			}
			enqueue(duResults, duFailures, duSkipped)
		})
		return
	}
	enqueue(nil, nil, nil)
}

// SetQuickCheckEnabled enables or disables the CRC pre-verify pass at runtime
// without restarting. Takes effect for the next job that enters post-processing.

// rebuildJobFromNZB reconstructs a queue.Job for a history entry by
// re-parsing the gzipped NZB backup recorded on it.
//
// This is where the article message-IDs come back. They are not in SQLite —
// job_files holds per-file metadata, durable_runs holds article INDICES, and
// neither holds the <id@host> strings a BODY command needs — and the job's
// manifest was unlinked when it finalized. The NZB is the only remaining copy,
// which is why the backup's name is recorded on the entry.
//
// The rebuilt job takes the entry's ID so its retained progress, its
// incomplete directory, and any later history entry all still line up.
func (app *Application) rebuildJobFromNZB(entry history.Entry) (*job.Job, dispatch.Header, error) {
	if entry.NZBBackup == "" {
		return nil, dispatch.Header{}, fmt.Errorf("app: retry %s: no NZB backup was recorded for this job", entry.NzoID)
	}
	adminDir := app.config.GetGeneral().AdminDir
	// filepath.Base defends against a stored value containing separators;
	// the column is written by us but is not a trust boundary we control
	// once the database is on disk.
	path := filepath.Join(adminDir, "nzb", filepath.Base(entry.NZBBackup))

	f, err := os.Open(path) //nolint:gosec // path is adminDir-rooted with the basename taken above
	if err != nil {
		return nil, dispatch.Header{}, fmt.Errorf("app: retry %s: open NZB backup %q: %w", entry.NzoID, entry.NZBBackup, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, dispatch.Header{}, fmt.Errorf("app: retry %s: read NZB backup %q: %w", entry.NzoID, entry.NZBBackup, err)
	}
	defer func() { _ = gz.Close() }()

	parsed, err := nzb.Parse(gz)
	if err != nil {
		return nil, dispatch.Header{}, fmt.Errorf("app: retry %s: parse NZB backup %q: %w", entry.NzoID, entry.NZBBackup, err)
	}

	pp := types.PPInherit
	if entry.PP != "" {
		if n, convErr := strconv.Atoi(entry.PP); convErr == nil {
			pp = n
		}
	}
	j, hdr, err := BuildIngestJob(app.config, parsed, entry.NzbName, types.FetchOptions{
		JobID:    entry.NzoID,
		NzbName:  entry.Name,
		Category: entry.Category,
		Script:   entry.Script,
		Password: entry.Password,
		PP:       pp,
	}, app.log)
	if err != nil {
		return nil, dispatch.Header{}, fmt.Errorf("app: retry %s: %w", entry.NzoID, err)
	}
	hdr.NZBBackup = entry.NZBBackup
	return j, hdr, nil
}

// RetryHistoryJob re-enqueues a failed history job for re-download.
//
// The job is rebuilt by re-parsing the NZB recorded on its history entry —
// the only place the article message-IDs survive once the manifest is
// unlinked at finalization — and then overlaid with the per-file progress
// retained for failed jobs, so only the articles that did not succeed are
// refetched. Where every article already resolved and only post-processing
// failed, the overlay is what sends the job straight back to post-processing
// instead of re-downloading it in full.
//
// Only failed entries are retryable, matching SABnzbd, whose
// get_incomplete_path returns a path only for status = Failed. A completed
// job has nothing to retry.
//
// The history entry is deleted on success.
func (app *Application) RetryHistoryJob(ctx context.Context, jobID string) error {
	entry, err := app.historyRepo.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if entry.Status != string(constants.StatusFailed) {
		return fmt.Errorf("app: retry %s: only failed jobs can be retried, this one is %q",
			jobID, entry.Status)
	}

	j, hdr, err := app.rebuildJobFromNZB(*entry)
	if err != nil {
		return err
	}
	var progressApplied bool
	retained, rErr := app.historyFileProgress(ctx, jobID)
	if rErr != nil {
		return fmt.Errorf("app: retry %s: restore retained progress: %w", jobID, rErr)
	}
	m, _ := j.Manifest()
	if len(retained) > 0 && m != nil && retainedMatchesManifest(retained, m) {
		if app.runs != nil {
			runs, runErr := app.runs.ForJob(ctx, jobID)
			if runErr != nil {
				return fmt.Errorf("app: retry %s: load runs: %w", jobID, runErr)
			}
			var files []int32
			for fi := range m.NumFiles() {
				files = append(files, int32(fi))
			}
			if err := j.ReplaceFromRuns(files, runs); err == nil {
				progressApplied = true
			}
		}
		for _, f := range retained {
			_ = j.RestoreFileMeta(f.FileIndex, f.Filename, f.Complete, f.AssembledCRC32, f.Fetch)
		}
	} else if len(retained) > 0 {
		app.log.Info("no usable retained progress for retry; downloading from scratch",
			"job", jobID)
	}

	if !progressApplied {
		if err := app.dropJobDurability(ctx, jobID); err != nil {
			return fmt.Errorf("app: retry %s: drop stale durability rows: %w", jobID, err)
		}
	}
	if app.historyRepo != nil && app.historyRepo.DB() != nil {
		if _, err := app.historyRepo.DB().ExecContext(ctx,
			"DELETE FROM failed_articles WHERE job_id = ?", jobID); err != nil {
			app.log.Warn("could not clear failed_articles for retry", "job", jobID, "err", err)
		}
	}
	j.ResetForRetry()
	if app.barrier != nil {
		app.barrier.ForgetJob(jobID)
	}
	if app.assembler != nil {
		if err := app.assembler.ForgetJob(ctx, jobID); err != nil {
			app.log.Warn("could not clear the assembler's completed-file tombstones for a "+
				"retry; articles for files this process already finished will be refused "+
				"as late duplicates until a restart",
				"job", jobID, "err", err)
		}
	}

	adminDir := app.config.GetGeneral().AdminDir
	mdir := manifestDir(adminDir)
	if err := os.MkdirAll(mdir, 0o750); err != nil {
		return fmt.Errorf("app: retry %s: mkdir manifests: %w", jobID, err)
	}
	if m, err := j.Manifest(); err == nil && m != nil {
		data, mErr := json.Marshal(m)
		if mErr != nil {
			return fmt.Errorf("app: retry %s: marshal manifest: %w", jobID, mErr)
		}
		mpath, pErr := manifestPath(adminDir, j.ID())
		if pErr != nil {
			return fmt.Errorf("app: retry %s: write manifest: %w", jobID, pErr)
		}
		if err := fsutil.WriteGzAtomicBytes(mpath, data); err != nil {
			return fmt.Errorf("app: retry %s: write manifest: %w", jobID, err)
		}
	}

	if app.dispatcher != nil {
		if err := app.dispatcher.Add(j, hdr); err != nil {
			return err
		}
	}

	if _, err := app.historyRepo.DeleteKeepingDurability(ctx, jobID); err != nil {
		app.log.Warn("retry requeued but its history entry could not be deleted; "+
			"the entry is stale until this attempt finalizes over it",
			"job", jobID, "err", err)
	}
	app.emit(Event{Type: "queue_updated"})
	app.emit(Event{Type: "history_updated"})
	if j.IsComplete() {
		msg := failMsgForJob(j)
		app.maybeFinalize(jobID, msg)
	}
	return nil
}

type retainedFile struct {
	FileIndex      int
	Complete       bool
	Fetch          job.FetchPolicy
	Filename       string
	AssembledCRC32 uint32
	ArticleCount   int
}

func (app *Application) historyFileProgress(ctx context.Context, jobID string) ([]retainedFile, error) {
	if app.historyRepo == nil || app.historyRepo.DB() == nil {
		return nil, nil
	}
	const q = `
SELECT file_index, complete, fetch_policy,
       COALESCE(filename, ''), COALESCE(assembled_crc32, 0), article_count
FROM history_job_files WHERE job_id = ? ORDER BY file_index ASC`
	rows, err := app.historyRepo.DB().QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("app: query history file progress %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []retainedFile
	for rows.Next() {
		var f retainedFile
		var complete, fetch int
		if err := rows.Scan(&f.FileIndex, &complete, &fetch,
			&f.Filename, &f.AssembledCRC32, &f.ArticleCount); err != nil {
			return nil, fmt.Errorf("app: scan history_job_file %s: %w", jobID, err)
		}
		f.Complete = complete != 0
		f.Fetch = job.FetchPolicy(fetch) //nolint:gosec // G115: fetch_policy is 0-2, fits in uint8
		out = append(out, f)
	}
	return out, rows.Err()
}

func retainedMatchesManifest(retained []retainedFile, m *job.Manifest) bool {
	if len(retained) != m.NumFiles() {
		return false
	}
	for i, f := range retained {
		if f.FileIndex != i {
			return false
		}
		lo, hi := m.FileRange(i)
		if f.ArticleCount != hi-lo {
			return false
		}
	}
	return true
}

// buildDownloaderOptions constructs a downloader.Options from the current
// app config. Used by both New() and ReloadDownloader() to ensure the same
// options are applied consistently.
func (app *Application) buildDownloaderOptions() downloader.Options {
	dl := app.config.GetDownloads()
	maxArtTries := dl.MaxArtTries
	maxArtOpt := dl.MaxArtOpt
	topOnly := dl.TopOnly
	noPenalties := dl.NoPenalties
	preCheck := dl.PreCheck
	propDelay := dl.PropagationDelay
	return downloader.Options{
		MaxArtTries:      maxArtTries,
		MaxArtOpt:        maxArtOpt,
		TopOnly:          topOnly,
		NoPenalties:      noPenalties,
		PreCheck:         preCheck,
		PropagationDelay: time.Duration(propDelay) * time.Minute,
		OnJobHopeless: func(jobID string) {
			var msg string
			if app.dispatcher != nil {
				if j, ok := app.dispatcher.Job(jobID); ok {
					msg = failMsgForJob(j)
				}
			}
			if msg == "" {
				msg = "Aborted: 80%+ of first articles failed (DMCA'd or expired)"
			}
			app.maybeFinalize(jobID, msg)
		},
	}
}

// WithDownloader returns an option that overrides the Application's downloader.
func WithDownloader(d Downloader) func(*Application) {
	return func(a *Application) {
		a.downloader = d
		if ds, ok := d.(DownloaderStats); ok {
			a.downloaderStats = ds
		}
	}
}

// WithLogger returns an option that overrides the Application's logger.
func WithLogger(log *slog.Logger) func(*Application) {
	return func(a *Application) { a.log = log }
}

// WithPostProcStages returns an option that overrides the post-processing stages.
func WithPostProcStages(stages []postproc.Stage) func(*Application) {
	return func(a *Application) { a.customStages = stages }
}

// WithVersion returns an option that sets the application build version.
func WithVersion(v string) func(*Application) {
	return func(a *Application) { a.version = v }
}

// WithEventEmitter returns an option that sets the real-time event broadcaster.
// If e is nil, the application defaults to a no-op dummy emitter.
func WithEventEmitter(e EventEmitter) func(*Application) {
	return func(a *Application) {
		if e != nil {
			a.emitter = e
		} else {
			a.emitter = dummyEmitter{}
		}
	}
}

// SetSpeedLimit updates the download speed limit. bytesPerSec <= 0 means unlimited.
func (app *Application) SetSpeedLimit(bytesPerSec int64) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloader != nil {
		app.downloader.SetSpeedLimit(bytesPerSec)
	}
}

// SetBandwidthMax updates the configured bandwidth ceiling reported to the UI.
func (app *Application) SetBandwidthMax(bytesPerSec int64) {
	app.bandwidthMax.Store(bytesPerSec)
}

// SetBandwidthPerc updates the configured bandwidth percentage reported to the UI.
func (app *Application) SetBandwidthPerc(perc int) {
	app.bandwidthPerc.Store(int32(perc)) //nolint:gosec // G115: perc is bounded 0-100
}

// SetDownloadDir updates the download directory used for new jobs.
// Already-queued jobs are unaffected since their paths were computed at
// enqueue time. The caller is responsible for creating the directory.
func (app *Application) SetDownloadDir(dir string) {
	app.mu.Lock()
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = dir
	})
	if app.pipeline != nil {
		app.pipeline.mu.Lock()
		app.pipeline.downloadDir = dir
		app.pipeline.mu.Unlock()
	}
	app.mu.Unlock()
	// --- No lock held below this line ---
	app.log.Info("download dir updated", "dir", dir)
}

// SetCompleteDir updates the complete directory used for new jobs.
// Already-queued jobs are unaffected since their FinalDir was computed at
// enqueue time. The caller is responsible for creating the directory.
func (app *Application) SetCompleteDir(dir string) {
	app.mu.Lock()
	app.config.With(func(c *config.Config) {
		c.General.CompleteDir = dir
	})
	app.mu.Unlock()
	// --- No lock held below this line ---
	app.log.Info("complete dir updated", "dir", dir)
}

// PauseDownloads cancels all in-flight fetch operations and flushes the
// speed meter so the UI graph drops to zero immediately. Call this in
// addition to queue.PauseAll() which only prevents new dispatch.
func (app *Application) PauseDownloads() {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloader != nil {
		app.downloader.Pause()
	}
}

// ResumeDownloads creates a fresh fetch context so workers can dial and
// fetch again, then pokes the dispatch loop.
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloader != nil {
		app.downloader.Resume()
	}
}

// DisconnectAll drops all idle NNTP connections. Workers stay alive and
// will re-dial lazily when new work arrives.
func (app *Application) DisconnectAll() {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloader != nil {
		app.downloader.DisconnectAll()
	}
}

// UnblockServer clears any active penalty on the named server, returning
// it to the dispatch pool immediately. Returns false if the server is not
// found or the downloader is not running.
func (app *Application) UnblockServer(name string) bool {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloader != nil {
		return app.downloader.UnblockServer(name)
	}
	return false
}

// handleLowDisk is invoked by the assembler worker goroutine when free space
// on the target directory drops below the configured threshold. It snapshots
// app.downloader rather than locking across Pause(): holding app.mu across
// Pause() would invert lock order against ReloadDownloader, which holds
// app.mu for its entire body including downloader.Stop().
func (app *Application) handleLowDisk(dir string, freeBytes int64) {
	app.mu.Lock()
	dl := app.downloader
	app.mu.Unlock()
	// --- No lock held below this line ---
	if dl != nil {
		dl.Pause()
	}
	app.log.Warn("low disk space, downloads paused",
		"dir", dir,
		"freeMB", freeBytes/(1024*1024))
}

// ServerStatus returns a point-in-time snapshot of all servers,
// including per-connection article activity. Returns nil when the
// downloader is not running.
func (app *Application) ServerStatus() []downloader.ServerSnapshot {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloaderStats != nil {
		return app.downloaderStats.ServerStatus()
	}
	return nil
}

func failMsgForJob(j *job.Job) string {
	if j == nil {
		return ""
	}
	return failMsgForCounters(j.Progress(), string(j.RepairState()), j.RecoveryBytes(), j.RecoveryFiles(), j.RepairState().Hopeless())
}

type failureByteCounters interface {
	FailedBytes() int64
	ContentFailedBytes() int64
	ExpectedBytes() int64
}

func failMsgForCounters(p failureByteCounters, state string, recBytes int64, recFiles int, hopeless bool) string {
	if p == nil {
		return ""
	}

	// Promoted scalars and JobProgress, never job.Manifest(): this runs from
	// the startup recovery walk over the dispatcher snapshot, where a job may have
	// no resident manifest and a manifest read would nil-deref.
	//
	// Two different failure figures, and the difference matters. The
	// all-failed check below asks "did everything we set out to fetch fail",
	// so its numerator and denominator must cover the same file set —
	// FailedBytes against ExpectedBytes. The repair checks after it ask "is
	// the damage larger than what can rebuild it", which is a question about
	// content only: a failed par2 file is lost capacity, not damage, and
	// there is nothing to repair it with or any reason to.
	totalFailed := p.FailedBytes()
	if totalFailed == 0 {
		return ""
	}
	contentFailed := p.ContentFailedBytes()

	// If ALL bytes the job set out to fetch failed, it is hopeless regardless
	// of par2. job.TotalBytes() would also count deferred recovery volumes
	// that were never dispatched and so can never appear in the failed total,
	// making this comparison impossible to satisfy for an on-demand-par2 job
	// whose content entirely failed — hence ExpectedBytes.
	if totalFailed >= p.ExpectedBytes() {
		return fmt.Sprintf(
			"Aborted: All articles failed (%.1f MB). Job is beyond repair",
			float64(totalFailed)/(1024*1024),
		)
	}

	failedMB := float64(contentFailed) / (1024 * 1024)

	// The verdict itself lives in queue.RepairStateFrom, which the dispatcher's
	// Early Health Gate and the queue listing also read. Only the wording of
	// each outcome belongs here: when these sites derived the comparison
	// separately they drifted, and OnJobHopeless passes this function's result
	// into maybeFinalize with no fallback string — so a dispatcher that
	// declares a job hopeless while this one still considers it repairable
	// finalizes the job with an empty reason and shows the user nothing.
	switch state {
	case string(job.RepairIntact), string(job.RepairPossible), string(job.RepairUnknown):
		// RepairIntact: only par2 failed, so there is nothing to repair and
		// the job merely cannot be verified. RepairPossible: within capacity.
		// RepairUnknown: par2 present but unrecognized, so capacity is
		// unmeasured rather than absent. All three proceed to
		// post-processing.
		return ""
	case string(job.RepairBeyondCapacity):
		// Capacity may be understated if some plainly-named par2 file also
		// carries slices, so this errs toward aborting.
		recoveryMB := float64(recBytes) / (1024 * 1024)
		return fmt.Sprintf(
			"Aborted: %.1f MB failed, exceeds repair capacity of %.1f MB (%d recovery volumes). Job is beyond repair",
			failedMB, recoveryMB, recFiles,
		)
	case string(job.RepairNoCapacity):
		return fmt.Sprintf(
			"Aborted: %.1f MB failed with no par2 files available. Job is beyond repair",
			failedMB,
		)
	default:
		// A state added to queue.RepairState that nobody taught this function
		// to word. Falling through to "" would be the precise failure the
		// comment above warns about: Hopeless() is opt-in per state and the
		// wording above is opt-in per state, as two separate lists, so a new
		// hopeless state would abort the job in the dispatcher while this
		// returned no reason at all — and OnJobHopeless hands that straight to
		// maybeFinalize with no fallback, showing the user nothing.
		//
		// TestFailMsgForJob_WordsEveryHopelessState fails when a state reaches
		// here; this arm only bounds the damage until someone reads it.
		if hopeless {
			return fmt.Sprintf(
				"Aborted: %.1f MB failed (%s). Job is beyond repair",
				failedMB, state,
			)
		}
		return ""
	}
}

// writeGzFile writes data to path as a gzip-compressed file using atomic
// temp+fsync+rename to prevent corruption on crash.
func writeGzFile(path string, data []byte) error {
	return fsutil.WriteGzAtomicBytes(path, data)
}

// writeNZBBackup stores rawNZB gzipped under nzbDir and returns the basename
// it used, which the caller records on the job so a retry can find it again.
//
// The file keeps the name the NZB was submitted under, because admin/nzb/ is
// browsed by hand to find an NZB to re-add or inspect. When that name is
// already taken — only reachable via a forced duplicate add — it takes the
// same ".1"/".2" suffix queue.UniqueName gives colliding job names, rather
// than overwriting and losing the earlier NZB.
//
// The stat-then-write window is the same benign TOCTOU AddJob already accepts
// for job names: this daemon is single-instance, and a lost race costs one
// overwritten backup rather than any queue state.
func writeNZBBackup(nzbDir, filename string, rawNZB []byte) (string, error) {
	base := filepath.Base(filename)
	name := uniqueName(base, func(candidate string) bool {
		_, err := os.Stat(filepath.Join(nzbDir, candidate+".gz"))
		return err == nil
	}) + ".gz"
	if err := writeGzFile(filepath.Join(nzbDir, name), rawNZB); err != nil {
		return "", err
	}
	return name, nil
}

// NNTPTestResult holds outcome metrics for an on-demand NNTP server test connection.
type NNTPTestResult struct {
	Latency                 time.Duration
	ConnectionLimitExceeded bool
}

// TestNNTPServer dials an NNTP server to verify connectivity and credentials.
//
//testdouble:allow SABnzbd test_nntp_server API implementation
func (a *Application) TestNNTPServer(ctx context.Context, cfg config.ServerConfig) (NNTPTestResult, error) {
	start := time.Now()
	log := slog.Default()
	if a != nil && a.log != nil {
		log = a.log.With("component", "nntp_test")
	}
	conn, err := nntp.Dial(ctx, cfg, nntp.WithLogger(log))
	if err != nil {
		return NNTPTestResult{
			ConnectionLimitExceeded: errors.Is(err, nntp.ErrServerUnavailable),
		}, err
	}
	_ = conn.Close() //nolint:errcheck // test connection; close error is irrelevant
	return NNTPTestResult{
		Latency: time.Since(start),
	}, nil
}
