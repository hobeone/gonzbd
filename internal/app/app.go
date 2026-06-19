// Package app wires the download pipeline: queue → downloader → decoder →
// assembler. It owns the lifecycle of each subsystem (Start, Shutdown) and
// bridges between them via a pipeline goroutine that decodes raw NNTP bodies
// and hands decoded parts to the assembler for pwrite-based out-of-order
// assembly.
package app

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// ErrAlreadyStarted is returned by Start on the second call to a live
// Application.
var ErrAlreadyStarted = errors.New("app: already started")

const defaultCheckpointInterval = 30 * time.Second

// Downloader defines the interface for the Usenet article downloader.
// Downloader defines the lifecycle and control interface for the Usenet article downloader.
type Downloader interface {
	Start(ctx context.Context) error
	Stop() error
	Completions() <-chan *downloader.ArticleResult
	SetSpeedLimit(bytesPerSec int64)
	SetDispatchOptions(maxArtTries, maxArtOpt int, topOnly bool, propagationDelay time.Duration)
	UnblockServer(name string) bool
	Pause()
	Resume()
	SetOnJobHopeless(cb func(jobID string))
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
	mu                 sync.Mutex
	config             *config.Config
	emitter            EventEmitter
	meter              *bpsmeter.Meter

	queue            *queue.Queue
	historyRepo      *history.Repository
	downloader       Downloader
	downloaderStats  DownloaderStats
	assembler        *assembler.Assembler
	postProcessor    *postproc.PostProcessor
	pipeline         *pipeline
	fileComplete     chan FileComplete
	jobComplete      chan JobComplete
	postProcComplete chan PostProcComplete
	notifyDispatcher *notifier.Dispatcher

	internalFileComplete chan FileComplete

	wg     sync.WaitGroup
	ctx    context.Context //nolint:containedctx // ctx is the app's lifecycle context, stored by design
	cancel context.CancelFunc

	started atomic.Bool
	stopped atomic.Bool

	bandwidthMax  atomic.Int64 // configured bandwidth ceiling in bytes/sec
	bandwidthPerc atomic.Int32 // configured bandwidth percentage (1-100)

	customStages     []postproc.Stage
	quickCheckStage  *postproc.QuickCheckStage
	repairStage      *postproc.RepairStage
	par2CleanupStage *postproc.Par2CleanupStage
	unpackStage      *postproc.UnpackStage
	finalizeStage    *postproc.FinalizeStage
	scriptStage      *postproc.ScriptStage
	sampleStage      *postproc.SampleCleanupStage
	deobfuscateStage *postproc.DeobfuscateStage
	cleanupStage     *postproc.ExtensionCleanupStage

	// directUnpackers maps jobID → active DirectUnpacker for jobs being
	// extracted during download. Protected by mu.
	directUnpackers map[string]*directunpack.DirectUnpacker

	// activeDU tracks the number of currently running DirectUnpackers.
	// Used to enforce DirectUnpackThreads concurrency limit.
	activeDU atomic.Int32
}

// SetNotifier injects a notification dispatcher for lifecycle events.
func (app *Application) SetNotifier(d *notifier.Dispatcher) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.notifyDispatcher = d
}

// New constructs an Application from cfg.
func New(cfg *config.Config, repo *history.Repository, opts ...func(*Application)) (*Application, error) {
	var dlDir, completeDir, adminDir string
	var writeCacheBytes, minFreeBytes int64
	var sanitize fsutil.SanitizeOptions
	var serversConfig []config.ServerConfig
	cfg.WithRead(func(c *config.Config) {
		dlDir = c.General.DownloadDir
		completeDir = c.General.CompleteDir
		adminDir = c.General.AdminDir
		writeCacheBytes = int64(c.Downloads.WriteCacheSize)
		minFreeBytes = int64(c.Downloads.MinFreeSpace)
		sanitize = c.Downloads.SanitizeOptions()
		serversConfig = c.Servers
	})

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
		fileComplete:         make(chan FileComplete, 16),
		internalFileComplete: make(chan FileComplete, 128),
		jobComplete:          make(chan JobComplete, 8),
		postProcComplete:     make(chan PostProcComplete, 8),
		directUnpackers:      make(map[string]*directunpack.DirectUnpacker),
	}
	for _, o := range opts {
		o(app)
	}
	if app.log == nil {
		app.log = slog.Default().With("component", "app")
	}
	log := app.log

	if app.meter == nil {
		app.meter = bpsmeter.NewMeter(10*time.Second, time.Now)
	}

	queueStateDir := filepath.Join(adminDir, "queue")
	q, err := queue.Load(queueStateDir)
	if err != nil {
		return nil, fmt.Errorf("app: load queue: %w", err)
	}
	app.queue = q

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
		realDL := downloader.New(q, servers, app.meter, app.buildDownloaderOptions(), log)
		app.downloader = realDL
		app.downloaderStats = realDL
	}

	// Apply initial bandwidth limit from config.
	var bandwidthMax int64
	var bandwidthPerc int
	app.config.WithRead(func(c *config.Config) {
		bandwidthMax = int64(c.Downloads.BandwidthMax)
		bandwidthPerc = int(c.Downloads.BandwidthPerc)
	})

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
		queue:       q,
		completions: app.downloader.Completions(),
		downloadDir: dlDir,
		sanitize:    sanitize,
		onJobHopeless: func(jobID string) {
			snap := q.SnapshotJob(jobID)
			if snap == nil {
				return
			}
			msg := failMsgForJob(snap)
			if msg == "" {
				msg = "Aborted: 80%+ of first articles failed (DMCA'd or expired)"
			}
			app.maybeFinalize(jobID, msg)
		},
		updateCh: make(chan completionSwap, 1),
		fileInfo: make(map[fileKey]assembler.FileInfo),
	}
	app.pipeline = p

	stages := app.customStages
	if stages == nil {
		probe := probeBinaries(context.Background(), cfg, log)
		built, err := buildStages(cfg, app.version, log, probe)
		if err != nil {
			return nil, err
		}
		stages = built.Stages
		app.quickCheckStage = built.QuickCheck
		app.repairStage = built.Repair
		app.par2CleanupStage = built.Par2Cleanup
		app.unpackStage = built.Unpack
		app.finalizeStage = built.Finalize
		app.scriptStage = built.Script
		app.sampleStage = built.SampleCleanup
		app.deobfuscateStage = built.Deobfuscate
		app.cleanupStage = built.ExtensionCleanup
	}
	pp := postproc.New(postproc.Options{
		Stages: stages,
		StatusUpdater: func(jobID string, status constants.Status) {
			_ = q.SetStatus(jobID, status)
		},
		OnOutput: func(jobID, tool, line string) {
			app.emit(Event{
				Type:  "postproc_output",
				NzoID: jobID,
				Tool:  tool,
				Line:  line,
			})
		},
		OnJobDone: app.finalizeJob,
		Logger:    log,
	})
	app.postProcessor = pp

	asm := assembler.New(assembler.Options{
		FileInfo:           p.resolveFileInfo,
		MarkArticlesDone:   q.MarkArticlesDone,
		MarkArticlesFailed: q.MarkArticlesFailed,
		SetWriteCursor:     q.SetFileWriteCursor,
		MinFreeBytes:       minFreeBytes,
		WriteCacheBytes:    writeCacheBytes,
		OnLowDisk: func(dir string, freeBytes int64) {
			app.downloader.Pause()
			app.log.Warn("low disk space, downloads paused",
				"dir", dir,
				"freeMB", freeBytes/(1024*1024))
		},
		OnFileComplete: func(jobID string, fileIdx int, fileCRC uint32) {
			fc := FileComplete{JobID: jobID, FileIdx: fileIdx, CRC32: fileCRC}
			select {
			case app.fileComplete <- fc:
			default:
			}
			select {
			case app.internalFileComplete <- fc:
			default:
				// Channel full — spawn goroutine to ensure delivery.
				go func() {
					select {
					case app.internalFileComplete <- fc:
					case <-app.ctx.Done():
						// App shutting down — discard to avoid blocking.
					}
				}()
			}
		},
	}, log)
	app.assembler = asm
	p.assembler = asm

	return app, nil
}

// finalizeJob is called by the post-processor when a job is done (success
// or failure). It builds a history entry, persists the job payload for
// retry support, writes to the history DB, removes the job from the
// active queue, fires WebSocket events, and dispatches notifications.
//
// This was extracted from the OnJobDone closure in New() to make the
// history-entry construction and notification logic independently testable.
func (app *Application) finalizeJob(job *postproc.Job) {
	entry := buildHistoryEntry(job)
	if err := app.persistAndCommit(app.log, entry, job); err != nil {
		return
	}
	app.fireCompletionNotification(entry)
}

// persistAndCommit saves the job payload to disk, writes the history entry to
// the database, removes the job from the queue, and broadcasts the finalization
// events. Returns a non-nil error if persistence failed and the job was kept in
// the queue for recovery (the error is already logged; callers can simply return).
func (app *Application) persistAndCommit(log *slog.Logger, entry history.Entry, job *postproc.Job) error {
	var adminDir string
	app.config.WithRead(func(c *config.Config) {
		adminDir = c.General.AdminDir
	})
	histJobsDir := filepath.Join(adminDir, "history", "jobs")
	if err := os.MkdirAll(histJobsDir, 0o750); err != nil {
		log.Warn("failed to create history jobs dir", "err", err)
	}
	jobPath := filepath.Join(histJobsDir, job.Queue.ID+".json.gz")
	if err := queue.SaveJob(jobPath, job.Queue); err != nil {
		log.Error("failed to save final job state; keeping job in queue",
			"job", job.Queue.ID, "err", err)
		app.emit(Event{Type: "queue_updated"})
		return err
	}
	if app.historyRepo != nil {
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := app.historyRepo.Add(dbCtx, entry); err != nil {
			log.Error("failed to add history entry; keeping job in queue for recovery",
				"job", job.Queue.ID, "err", err)
			dbCancel()
			_ = os.Remove(jobPath) // clean up the orphaned payload file
			app.emit(Event{Type: "queue_updated"})
			return err
		}
		dbCancel()
	}
	if err := app.queue.Remove(job.Queue.ID); err != nil {
		log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
	}
	select {
	case app.postProcComplete <- PostProcComplete{JobID: job.Queue.ID}:
	default:
	}
	// job_finalized signals a queue→history transition so both stores
	// refresh from a single trigger and reach the new state together.
	app.emit(Event{Type: "job_finalized", NzoID: job.Queue.ID})
	return nil
}

// fireCompletionNotification sends a push notification for a finished job.
// Runs with a bounded context so a slow notification sink can't block the
// postproc worker indefinitely.
func (app *Application) fireCompletionNotification(entry history.Entry) {
	if app.notifyDispatcher == nil {
		return
	}
	evtType := notifier.PostProcessingComplete
	title := "Download completed"
	if entry.Status == "Failed" {
		evtType = notifier.PostProcessingFailed
		title = "Download failed"
	}
	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	app.notifyDispatcher.Dispatch(notifyCtx, notifier.Event{
		Type:      evtType,
		Title:     title,
		Body:      entry.Name,
		JobName:   entry.Name,
		Timestamp: time.Now(),
	})
	notifyCancel()
}

// Queue returns the application's download queue.
func (app *Application) Queue() *queue.Queue { return app.queue }

// Speed returns the current aggregate download speed in bytes/sec, or 0
// when downloading is idle or the downloader stats interface has not been wired yet.
func (app *Application) Speed() float64 { //nocover: trivial delegate
	if app.downloaderStats == nil {
		return 0
	}
	return app.downloaderStats.Speed()
}

// AddJob validates, deduplicates, and enqueues a new download job. If force
// is false and a duplicate is detected, the job is added in a paused state.
func (app *Application) AddJob(ctx context.Context, job *queue.Job, rawNZB []byte, force bool) error {
	var adminDir string
	app.config.WithRead(func(c *config.Config) {
		adminDir = c.General.AdminDir
	})
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		return fmt.Errorf("app: mkdir admin nzb: %w", err)
	}

	isDuplicate := false
	dupReason := ""
	if app.queue.ExistsByMD5(job.MD5) {
		isDuplicate = true
		dupReason = "found in active queue (MD5)"
	}
	if !isDuplicate && app.historyRepo != nil {
		results, err := app.historyRepo.Search(ctx, history.SearchOptions{MD5Sum: job.MD5})
		if err == nil && len(results) > 0 {
			isDuplicate = true
			dupReason = fmt.Sprintf("found in history DB (MD5: %q)", results[0].NzoID)
		}
	}
	if !isDuplicate && job.Filename != "" {
		base := filepath.Base(job.Filename)
		// Check for gzipped backup (current format) and uncompressed (legacy).
		if _, err := os.Stat(filepath.Join(nzbDir, base+".gz")); err == nil {
			isDuplicate = true
			dupReason = "found in admin/nzb/ backup dir (filename)"
		} else if _, err := os.Stat(filepath.Join(nzbDir, base)); err == nil {
			isDuplicate = true
			dupReason = "found in admin/nzb/ backup dir (filename, legacy)"
		}
	}
	if isDuplicate {
		app.log.Info("duplicate NZB detected", "filename", job.Filename, "md5", job.MD5, "reason", dupReason, "forced", force)
		if !force {
			job.Status = constants.StatusPaused
			job.Warning = "Duplicate NZB"
		} else {
			job.Warning = "Duplicate NZB (Forced)"
		}
	}
	// Pick a name not already taken in the queue or on disk. queue.Add
	// re-checks under its write lock (authoritative), so the small TOCTOU
	// window here is limited to filesystem collisions which are benign.
	var downloadDir, completeDir string
	var categories []config.CategoryConfig
	app.config.WithRead(func(c *config.Config) {
		downloadDir = c.General.DownloadDir
		completeDir = c.General.CompleteDir
		categories = c.Categories
	})
	job.Name = queue.UniqueName(job.Name, func(name string) bool {
		if app.queue.ExistsByName(name) {
			return true
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
	if !isDuplicate && job.Filename != "" {
		backupPath := filepath.Join(nzbDir, filepath.Base(job.Filename)+".gz")
		if err := writeGzFile(backupPath, rawNZB); err != nil {
			app.log.Warn("failed to write gzipped NZB backup", "path", backupPath, "err", err)
		}
	}
	addStatus := job.Status // snapshot before q.Add notifies the dispatcher
	if err := app.queue.Add(job); err != nil {
		return fmt.Errorf("app: add to queue: %w", err)
	}
	app.emit(Event{Type: "queue_updated"})
	app.log.Info("job added", "name", job.Name, "id", job.ID, "status", addStatus)
	return nil
}

// RemoveJob cancels and removes a job from the queue, deleting its download directory.
func (app *Application) RemoveJob(ctx context.Context, id string, deleteFiles bool) error {
	snap := app.queue.SnapshotJob(id)
	if snap == nil {
		return fmt.Errorf("job %q not found", id)
	}
	// Abort any active DirectUnpacker for this job before removing files.
	app.mu.Lock()
	if du, ok := app.directUnpackers[id]; ok {
		du.Abort()
		delete(app.directUnpackers, id)
		app.activeDU.Add(-1)
	}
	app.mu.Unlock()

	// Cancel in-flight post-processing and assembler file handles before
	// removing files to prevent the PP from operating on a deleted directory.
	app.postProcessor.Cancel(id)
	_ = app.assembler.CancelJob(ctx, id)
	// Remove from queue and pipeline so no more articles are dispatched.
	if err := app.queue.Remove(id); err != nil {
		return err
	}
	app.pipeline.forgetJob(id)
	if deleteFiles {
		var downloadDir string
		app.config.WithRead(func(c *config.Config) {
			downloadDir = c.General.DownloadDir
		})
		path := filepath.Join(downloadDir, snap.Name)
		if err := safeDeleteDir(path, downloadDir); err != nil {
			app.log.Warn("failed to delete job directory", "path", path, "err", err)
		}
	}
	app.emit(Event{Type: "queue_updated"})

	// Disconnect NNTP servers if no downloadable jobs remain.
	if !app.queue.HasDownloadableJobs() {
		app.downloader.DisconnectAll()
	}
	return nil
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
		var downloadDir, completeDir string
		app.config.WithRead(func(c *config.Config) {
			downloadDir = c.General.DownloadDir
			completeDir = c.General.CompleteDir
		})
		// A history job's files may live under the complete dir (finished)
		// or the download dir (failed); allow either, refuse anything else.
		if err := safeDeleteDir(entry.Path, completeDir, downloadDir); err != nil {
			app.log.Warn("failed to delete history job directory", "path", entry.Path, "err", err)
		}
	}
	if _, err := app.historyRepo.Delete(ctx, id); err != nil {
		return err
	}
	app.emit(Event{Type: "history_updated"})
	return nil
}

// GetHistory retrieves a single history entry by ID.
func (app *Application) GetHistory(ctx context.Context, id string) (*history.Entry, error) {
	if app.historyRepo == nil {
		return nil, errors.New("history repository not wired")
	}
	return app.historyRepo.Get(ctx, id)
}

// FileComplete returns the channel signalled when a file finishes assembly.
func (app *Application) FileComplete() <-chan FileComplete { return app.fileComplete }

// JobComplete returns the channel signalled when all files in a job are done.
func (app *Application) JobComplete() <-chan JobComplete { return app.jobComplete }

// PostProcComplete returns the channel signalled when post-processing finishes.
func (app *Application) PostProcComplete() <-chan PostProcComplete { return app.postProcComplete }

// Start launches the download pipeline, assembler, and background goroutines.
// It blocks until all components are running. Call Shutdown to stop.
// If Start returns an error it may be retried; the application is left in a
// clean state with started=false so a subsequent Start call is allowed.
func (app *Application) Start(ctx context.Context) error {
	if !app.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	// Reset started on any failure so the caller can retry. The deferred
	// function is cancelled by setting succeeded=true before returning nil.
	succeeded := false
	defer func() {
		if !succeeded {
			app.started.Store(false)
		}
	}()

	app.ctx, app.cancel = context.WithCancel(ctx)
	if err := app.assembler.Start(app.ctx); err != nil {
		return err
	}
	// Reset transient download state: Downloading → Queued, Emitted → false.
	// On a cold restart these flags are stale — the old downloader's
	// in-flight articles are long gone.
	app.queue.ClearAllEmitted()
	if err := app.downloader.Start(app.ctx); err != nil {
		_ = app.assembler.Stop()
		return err
	}
	if err := app.postProcessor.Start(app.ctx); err != nil {
		_ = app.downloader.Stop()
		_ = app.assembler.Stop()
		return err
	}
	app.pipeline.ctx = app.ctx // must be set before goroutine launch (setCompletions reads it)
	app.wg.Go(func() { app.pipeline.run(app.ctx) })
	app.wg.Go(func() { app.watchCompletions(app.ctx) })
	interval := app.checkpointInterval
	if interval <= 0 {
		interval = defaultCheckpointInterval
	}
	app.wg.Go(func() { app.runCheckpoint(app.ctx, interval) })
	app.wg.Go(func() { app.runMetricsPush(app.ctx) })
	app.log.Info("application started")

	for _, snap := range app.queue.Snapshot() {
		if !snap.IsComplete() {
			continue
		}
		if app.historyRepo != nil {
			dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := app.historyRepo.Get(dbCtx, snap.ID)
			dbCancel()
			if err == nil {
				app.log.Info("found completed job in history but still in queue, removing", "jobID", snap.ID)
				if rmErr := app.queue.Remove(snap.ID); rmErr != nil {
					app.log.Error("failed to remove duplicate job from queue", "jobID", snap.ID, "err", rmErr)
				}
				continue
			} else if !errors.Is(err, history.ErrNotFound) {
				app.log.Error("failed to check history for job", "jobID", snap.ID, "err", err)
			}
		}

		failMsg := failMsgForJob(snap)
		if snap.PostProc {
			// Crash recovery: PostProc was already set before the process
			// died. We snapshot the job to decouple the post-processor
			// from the queue's live pointer (preventing data races with
			// concurrent API mutations). SetPostProcStarted is bypassed
			// since it's already true.
			if app.postProcessor.Has(snap.ID) {
				continue
			}
			jobSnap := app.queue.SnapshotJob(snap.ID)
			if jobSnap == nil {
				continue
			}
			app.enqueuePostProc(jobSnap, failMsg)
			continue
		}
		app.maybeFinalize(snap.ID, failMsg)
	}

	succeeded = true
	return nil
}

// Shutdown stops the downloader, post-processor, and assembler, flushes the
// cache, and persists the queue to disk. Safe to call multiple times.
//
// Ordering matters:
//  1. Stop the downloader — no new articles are dispatched.
//  2. Stop the assembler — drains in-flight writes and delivers any remaining
//     OnFileComplete events to watchCompletions, which is still running.
//  3. Cancel the context — watchCompletions exits.
//  4. Wait for background goroutines to finish.
//  5. Stop the post-processor, save queue.
func (app *Application) Shutdown() error {
	if !app.started.Load() || !app.stopped.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	if err := app.downloader.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("downloader stop: %w", err))
	}

	// Abort all active DirectUnpackers before stopping the assembler.
	// This kills unrar subprocesses and cleans up partial extracts.
	app.mu.Lock()
	for id, du := range app.directUnpackers {
		du.Abort()
		delete(app.directUnpackers, id)
		app.activeDU.Add(-1)
	}
	app.mu.Unlock()

	if err := app.assembler.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("assembler stop: %w", err))
	}
	app.cancel()
	app.wg.Wait()
	if err := app.postProcessor.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("postprocessor stop: %w", err))
	}
	var adminDir string
	app.config.WithRead(func(c *config.Config) {
		adminDir = c.General.AdminDir
	})
	if err := app.queue.Save(filepath.Join(adminDir, "queue")); err != nil {
		errs = append(errs, fmt.Errorf("queue save: %w", err))
	}
	return errors.Join(errs...)
}

func (app *Application) runCheckpoint(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !app.queue.IsDirty() {
				continue
			}
			var adminDir string
			app.config.WithRead(func(c *config.Config) {
				adminDir = c.General.AdminDir
			})
			_ = app.queue.Save(filepath.Join(adminDir, "queue"))
		}
	}
}

func (app *Application) watchCompletions(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Drain any pending completions so they're applied to
			// the queue before it's saved to disk during shutdown.
			app.drainCompletions(ctx)
			return
		case fc := <-app.internalFileComplete:
			app.handleFileComplete(ctx, fc)
		}
	}
}

// handleFileComplete processes a single file completion event.
func (app *Application) handleFileComplete(ctx context.Context, fc FileComplete) {
	// Store the assembled CRC32 on the queue's JobFile so it survives
	// serialization and is available during post-processing QuickCheck.
	if fc.CRC32 != 0 {
		if err := app.queue.SetFileCRC32(fc.JobID, fc.FileIdx, fc.CRC32); err != nil {
			app.log.Warn("set file CRC32 failed",
				"job", fc.JobID, "fileidx", fc.FileIdx, "err", err)
		}
	}
	if err := app.queue.MarkFileComplete(fc.JobID, fc.FileIdx); err != nil {
		return
	}
	app.emit(Event{Type: "queue_updated"})

	// DirectUnpack: feed completed RAR volumes to the unpacker for
	// streaming extraction during download.
	var directUnpack, enableUnrar bool
	app.config.WithRead(func(c *config.Config) {
		directUnpack = c.PostProc.DirectUnpack
		enableUnrar = c.PostProc.EnableUnrar
	})
	if directUnpack && enableUnrar {
		app.maybeDirectUnpack(fc)
	}

	snap := app.queue.SnapshotJob(fc.JobID)
	if snap != nil && snap.IsComplete() {
		if app.maybeReleaseRecoveryVolumes(ctx, fc.JobID, snap) {
			return // downloader will fetch recovery volumes
		}
		app.maybeFinalize(fc.JobID, failMsgForJob(snap))
	}
}

// maybeReleaseRecoveryVolumes checks whether a completed job with deferred par2
// recovery volumes needs repair. If so it un-defers the volumes, broadcasts a
// queue update, and returns true — the caller must not finalize yet (the
// downloader will fetch the volumes and trigger another completion event).
//
// Returns false when: there are no deferred volumes, the data verifies clean,
// or un-deferral itself fails (in which case we fall through to finalize without
// recovery volumes, matching the pre-on-demand-par2 behaviour).
func (app *Application) maybeReleaseRecoveryVolumes(ctx context.Context, jobID string, snap *queue.Job) bool {
	if ctx.Err() != nil {
		return false
	}
	if !snap.HasDeferredPar2() || snap.Par2Recovered {
		return false
	}
	var downloadDir string
	app.config.WithRead(func(c *config.Config) {
		downloadDir = c.General.DownloadDir
	})
	dir := filepath.Join(downloadDir, snap.Name)
	needsRecovery, reason := par2NeedsRecovery(dir, snap.Files, app.log)
	if !needsRecovery {
		app.log.Info("on-demand par2: verified clean, skipping recovery volumes", "job", jobID)
		_ = app.queue.DiscardDeferredPar2(jobID)
		return false
	}
	_ = app.queue.SetPar2ReleaseReason(jobID, reason)
	idxs := snap.DeferredRecoveryIndices()
	if err := app.queue.UndeferRecoveryVolumes(jobID, idxs); err != nil {
		app.log.Warn("on-demand par2: un-defer failed; finalizing without recovery volumes",
			"job", jobID, "err", err)
		return false
	}
	app.log.Info("on-demand par2: repair needed, fetching recovery volumes",
		"job", jobID, "volumes", len(idxs), "reason", reason)
	app.emit(Event{Type: "queue_updated"})
	return true
}

// par2NeedsRecovery reports whether a completed job needs its deferred par2
// recovery volumes downloaded for repair. It mirrors the post-processing
// QuickCheck stage: it parses the par2 index files already on disk in dir and
// compares them against the assembled CRC32s captured during download. Repair
// is needed when any par2-tracked file is corrupt (Mismatched), failed to
// download (NoCRC), or could not be matched (Unverified). When no usable par2
// index is on disk (e.g. the index itself failed to download), it returns true
// so the recovery volumes are fetched — the safe, today's-behaviour fallback.
func par2NeedsRecovery(dir string, files []queue.JobFile, log *slog.Logger) (needsRecovery bool, reason string) {
	sets, err := par2.FindPar2Files(dir)
	if err != nil || len(sets) == 0 {
		reason := "no usable par2 index found to verify against"
		if err != nil {
			reason = fmt.Sprintf("no usable par2 index found (err: %v)", err)
		}
		log.Info("on-demand par2: no usable par2 index to verify against; fetching recovery volumes",
			"dir", dir, "err", err)
		return true, reason
	}
	assembled := make([]par2.AssembledFile, len(files))
	for i, jf := range files {
		name := jf.Subject
		if jf.Filename != "" {
			name = jf.Filename
		}
		assembled[i] = par2.AssembledFile{
			FileName: name,
			CRC32:    jf.AssembledCRC32,
			FileSize: jf.Bytes,
		}
	}
	r := par2.VerifyCRCs(assembled, sets, log)

	// If none of our downloaded/assembled files are tracked by the PAR2 set,
	// then the PAR2 set protects the extracted contents (Layout B)
	// rather than the RAR files themselves. We cannot verify RARs
	// against it, so we should skip fetching recovery volumes.
	if r.Matched == 0 && r.Mismatched == 0 && r.NoCRC == 0 {
		log.Info("on-demand par2: no downloaded files are tracked by the par2 index; skipping recovery volumes",
			"dir", dir)
		return false, ""
	}

	needsRepair := r.Mismatched+r.NoCRC+r.Unverified > 0
	if !needsRepair {
		return false, ""
	}

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

	return true, strings.Join(parts, "; ")
}

// drainCompletions processes all buffered events on internalFileComplete.
func (app *Application) drainCompletions(ctx context.Context) {
	for {
		select {
		case fc := <-app.internalFileComplete:
			app.handleFileComplete(ctx, fc)
		default:
			return
		}
	}
}

// maybeDirectUnpack feeds a completed file to the DirectUnpacker for the
// job, creating one if this is the first RAR volume for that job.
func (app *Application) maybeDirectUnpack(fc FileComplete) {
	snap := app.queue.SnapshotJob(fc.JobID)
	if snap == nil || snap.PostProc {
		return
	}
	// Skip DU for jobs that don't want unpacking (PP < 2) or have
	// a password (DU would fail on the password and fall back anyway).
	if snap.PP < 2 {
		return
	}
	if snap.Password != "" {
		return
	}
	if fc.FileIdx < 0 || fc.FileIdx >= len(snap.Files) {
		return
	}
	filename := snap.Files[fc.FileIdx].Subject
	_, vol := directunpack.AnalyzeRarFilename(filename)
	if vol == 0 {
		return // not a RAR volume
	}

	// Resolve the on-disk path from the pipeline's file info cache.
	info, err := app.pipeline.resolveFileInfo(fc.JobID, fc.FileIdx)
	if err != nil {
		app.log.Debug("directunpack: cannot resolve file path",
			"job", fc.JobID, "fileidx", fc.FileIdx, "err", err)
		return
	}

	app.mu.Lock()
	du, exists := app.directUnpackers[fc.JobID]
	if !exists {
		var limit int
		var downloadDirBase string
		app.config.WithRead(func(c *config.Config) {
			limit = c.PostProc.DirectUnpackThreads
			downloadDirBase = c.General.DownloadDir
		})
		if limit > 0 && int(app.activeDU.Load()) >= limit {
			app.mu.Unlock()
			app.log.Debug("directunpack: skipping, concurrency limit reached",
				"job", fc.JobID, "active", app.activeDU.Load(), "limit", limit)
			return
		}
		downloadDir := filepath.Join(downloadDirBase, snap.Name)
		du = directunpack.New(
			app.log.With("component", "directunpack", "job", fc.JobID),
			fc.JobID, downloadDir, downloadDir,
			app.buildDirectUnpackOpts(),
		)
		// Provide all filenames so the DU can compute total volume counts.
		allNames := make([]string, len(snap.Files))
		for i := range snap.Files {
			allNames[i] = snap.Files[i].Subject
		}
		du.SetAllFilenames(allNames)
		app.directUnpackers[fc.JobID] = du
		app.activeDU.Add(1)
	}
	app.mu.Unlock()

	du.Add(app.ctx, filename, info.Path)
}

// buildDirectUnpackOpts constructs DirectUnpack options from the app config.
func (app *Application) buildDirectUnpackOpts() directunpack.Options {
	var flatUnpack, overwriteFiles, ignoreUnrarDates bool
	app.config.WithRead(func(c *config.Config) {
		flatUnpack = c.PostProc.FlatUnpack
		overwriteFiles = c.PostProc.OverwriteFiles
		ignoreUnrarDates = c.PostProc.IgnoreUnrarDates
	})
	return directunpack.Options{
		Password:         "", // per-job passwords are pre-checked; DU skips password jobs
		OneFolder:        flatUnpack,
		OverwriteFiles:   overwriteFiles,
		IgnoreUnrarDates: ignoreUnrarDates,
	}
}

func (app *Application) maybeFinalize(jobID, failMsg string) {
	started, err := app.queue.SetPostProcStarted(jobID)
	if err == nil && started {
		// Force an immediate queue save so the PostProc=true flag survives
		// a crash. Without this, a crash between job completion and the
		// next periodic checkpoint (~30s) would lose the flag, causing
		// articles to be re-downloaded on restart instead of entering
		// post-processing via crash recovery.
		var adminDir string
		app.config.WithRead(func(c *config.Config) {
			adminDir = c.General.AdminDir
		})
		if saveErr := app.queue.Save(filepath.Join(adminDir, "queue")); saveErr != nil {
			app.log.Warn("forced queue save on job completion failed", "job", jobID, "err", saveErr)
		}
		// Snapshot the job to decouple the post-processor from the
		// queue's live pointer. The PP may hold this for minutes during
		// repair/unpack; if the API mutates the queue's copy (Pause,
		// Resume), the snapshot is unaffected, preventing data races.
		snap := app.queue.SnapshotJob(jobID)
		if snap == nil {
			app.log.Warn("maybeFinalize: job disappeared", "job", jobID)
			return
		}
		app.enqueuePostProc(snap, failMsg)
	}
}

func (app *Application) enqueuePostProc(job *queue.Job, failMsg string) {
	// Mark the download phase as finished so OnJobDone can compute
	// download duration accurately, excluding post-processing time.
	job.DownloadFinished = time.Now()

	// Release cached file info for this job; the assembler no longer
	// needs it, and keeping it around leaks memory across many downloads.
	app.pipeline.forgetJob(job.ID)

	var downloadDirBase, completeDir string
	var categories []config.CategoryConfig
	var sanitize fsutil.SanitizeOptions
	app.config.WithRead(func(c *config.Config) {
		downloadDirBase = c.General.DownloadDir
		completeDir = c.General.CompleteDir
		categories = c.Categories
		sanitize = c.Downloads.SanitizeOptions()
	})
	downloadDir := filepath.Join(downloadDirBase, job.Name)

	// Log the handoff from download → postproc. This is the "entering
	// postproc" bookend; processJob logs the "exiting" bookend.
	var dlDuration time.Duration
	if !job.DownloadStarted.IsZero() {
		dlDuration = job.DownloadFinished.Sub(job.DownloadStarted)
	}
	app.log.Info("postproc: job entering pipeline",
		"job", job.ID,
		"name", job.Name,
		"category", job.Category,
		"download_dir", downloadDir,
		"download_duration", dlDuration.Round(time.Second),
		"total_bytes", job.TotalBytes,
		"failed_bytes", job.FailedBytes,
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
				"job", job.ID,
				"file", e.Name(),
				"size", sz,
				"dir", e.IsDir(),
			)
		}
	}

	cat := config.FindCategory(categories, job.Category)
	catDir := cat.Dir
	// P6: Category dir trailing '*' suppresses the per-job subfolder.
	// Files go directly into the category directory ("flat layout").
	// e.g. catDir="movies*" → complete_dir/movies/file.mkv
	//      catDir="movies"  → complete_dir/movies/JobName/file.mkv
	//      catDir="movies"  → complete_dir/movies/JobName/file.mkv
	flatLayout := strings.HasSuffix(catDir, "*")
	if flatLayout {
		catDir = strings.TrimSuffix(catDir, "*")
	}
	finalDir := filepath.Join(completeDir, catDir, job.Name)
	if flatLayout {
		finalDir = filepath.Join(completeDir, catDir)
	}
	// Collect DirectUnpack results (if any) before enqueuing post-processing.
	// Wait() blocks until any in-progress extraction finishes.
	var duResults map[string]directunpack.SuccessSet
	var duFailures map[string]directunpack.FailedSet
	var duSkipped map[string]directunpack.SkippedSet
	app.mu.Lock()
	du := app.directUnpackers[job.ID]
	if du != nil {
		app.activeDU.Add(-1)
	}
	delete(app.directUnpackers, job.ID)
	app.mu.Unlock()
	if du != nil {
		du.Wait()
		duResults = du.Results()
		duFailures = du.Failures()
		duSkipped = du.Skipped()
		if len(duResults) > 0 {
			app.log.Info("directunpack: passing results to postproc",
				"job", job.ID, "sets", len(duResults))
		}
		if len(duFailures) > 0 {
			app.log.Warn("directunpack: passing failures to postproc",
				"job", job.ID, "failed_sets", len(duFailures))
		}
		if len(duSkipped) > 0 {
			app.log.Info("directunpack: passing skipped sets to postproc",
				"job", job.ID, "skipped_sets", len(duSkipped))
		}
	}

	app.postProcessor.Process(&postproc.Job{
		Queue:                job,
		DownloadDir:          downloadDir,
		FinalDir:             finalDir,
		Sanitize:             sanitize,
		FailMsg:              failMsg,
		DirectUnpackSets:     duResults,
		DirectUnpackFailures: duFailures,
		DirectUnpackSkipped:  duSkipped,
	})
	select {
	case app.jobComplete <- JobComplete{JobID: job.ID}:
	default:
	}
}

// PausePostProcessor pauses the post-processing pipeline.
func (app *Application) PausePostProcessor() {
	app.postProcessor.Pause()
}

// ResumePostProcessor resumes the post-processing pipeline.
func (app *Application) ResumePostProcessor() {
	app.postProcessor.Resume()
}

// SetQuickCheckEnabled enables or disables the CRC pre-verify pass at runtime
// without restarting. Takes effect for the next job that enters post-processing.

// RetryHistoryJob re-enqueues a completed/failed history job for re-download.
// Failed articles are reset; the history entry is deleted on success.
func (app *Application) RetryHistoryJob(ctx context.Context, jobID string) error {
	_, err := app.historyRepo.Get(ctx, jobID)
	if err != nil {
		return err
	}
	var adminDir string
	app.config.WithRead(func(c *config.Config) {
		adminDir = c.General.AdminDir
	})
	jobPath := filepath.Join(adminDir, "history", "jobs", jobID+".json.gz")
	job, err := queue.LoadJob(jobPath)
	if err != nil {
		return err
	}
	job.Status = constants.StatusQueued
	job.PostProc = false
	job.DownloadStarted = time.Time{}
	job.DownloadFinished = time.Time{}
	job.ServerStats = nil
	job.FailedBytes = 0
	for fi := range job.Files {
		file := &job.Files[fi]
		anyReset := false
		for ai := range file.Articles {
			art := &file.Articles[ai]
			if art.Failed {
				art.Done = false
				art.Failed = false
				job.RemainingBytes += int64(art.Bytes)
				anyReset = true
			}
		}
		if anyReset {
			file.Complete = false
		}
	}
	if err := app.queue.Add(job); err != nil {
		return err
	}
	_, _ = app.historyRepo.Delete(ctx, jobID)
	app.emit(Event{Type: "queue_updated"})
	app.emit(Event{Type: "history_updated"})
	snap := app.queue.SnapshotJob(jobID)
	if snap != nil && snap.IsComplete() {
		app.maybeFinalize(jobID, failMsgForJob(snap))
	}
	return nil
}

// buildDownloaderOptions constructs a downloader.Options from the current
// app config. Used by both New() and ReloadDownloader() to ensure the same
// options are applied consistently.
func (app *Application) buildDownloaderOptions() downloader.Options {
	q := app.queue
	var maxArtTries, maxArtOpt int
	var topOnly, noPenalties, preCheck bool
	var propDelay int
	app.config.WithRead(func(c *config.Config) {
		maxArtTries = c.Downloads.MaxArtTries
		maxArtOpt = c.Downloads.MaxArtOpt
		topOnly = c.Downloads.TopOnly
		noPenalties = c.Downloads.NoPenalties
		preCheck = c.Downloads.PreCheck
		propDelay = c.Downloads.PropagationDelay
	})
	return downloader.Options{
		MaxArtTries:      maxArtTries,
		MaxArtOpt:        maxArtOpt,
		TopOnly:          topOnly,
		NoPenalties:      noPenalties,
		PreCheck:         preCheck,
		PropagationDelay: time.Duration(propDelay) * time.Minute,
		OnJobHopeless: func(jobID string) {
			snap := q.SnapshotJob(jobID)
			if snap == nil {
				return
			}
			app.maybeFinalize(jobID, failMsgForJob(snap))
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

// WithCheckpointInterval returns an option that sets the queue persistence interval.
func WithCheckpointInterval(d time.Duration) func(*Application) {
	return func(a *Application) { a.checkpointInterval = d }
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
	defer app.mu.Unlock()
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = dir
	})
	if app.pipeline != nil {
		app.pipeline.mu.Lock()
		app.pipeline.downloadDir = dir
		app.pipeline.mu.Unlock()
	}
	app.log.Info("download dir updated", "dir", dir)
}

// SetCompleteDir updates the complete directory used for new jobs.
// Already-queued jobs are unaffected since their FinalDir was computed at
// enqueue time. The caller is responsible for creating the directory.
func (app *Application) SetCompleteDir(dir string) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.config.With(func(c *config.Config) {
		c.General.CompleteDir = dir
	})
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

// ServerStatus returns a point-in-time snapshot of all servers,
// including per-connection article activity. Returns nil when the
// downloader is not running.
func (app *Application) ServerStatus() []downloader.ServerSnapshot { //nocover: trivial delegate
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloaderStats != nil {
		return app.downloaderStats.ServerStatus()
	}
	return nil
}

func failMsgForJob(job *queue.Job) string {
	if job.FailedBytes == 0 {
		return ""
	}

	failedMB := float64(job.FailedBytes) / (1024 * 1024)

	// If ALL bytes in the job failed, it's hopeless regardless of PAR2.
	if job.FailedBytes >= job.TotalBytes {
		return fmt.Sprintf(
			"Aborted: All articles failed (%.1f MB). Job is beyond repair",
			failedMB,
		)
	}

	// If PAR2 files exist and the failure exceeds repair capacity, abort.
	if job.Par2Bytes > 0 && job.FailedBytes > job.Par2Bytes {
		par2MB := float64(job.Par2Bytes) / (1024 * 1024)
		return fmt.Sprintf(
			"Aborted: %.1f MB failed, exceeds repair capacity of %.1f MB (%d par2 files). Job is beyond repair",
			failedMB, par2MB, job.Par2Files,
		)
	}

	// No PAR2 files at all and there are failures — can't repair.
	if job.Par2Bytes == 0 && job.FailedBytes > 0 {
		return fmt.Sprintf(
			"Aborted: %.1f MB failed with no par2 files available. Job is beyond repair",
			failedMB,
		)
	}

	// Partial failure within repair capacity — let post-processing try.
	return ""
}

// writeGzFile writes data to path as a gzip-compressed file using atomic
// temp+fsync+rename to prevent corruption on crash.
func writeGzFile(path string, data []byte) error {
	return fsutil.WriteAtomic(path, func(w io.Writer) error {
		gz := gzip.NewWriter(w)
		if _, err := gz.Write(data); err != nil {
			_ = gz.Close()
			return fmt.Errorf("gzip write: %w", err)
		}
		return gz.Close()
	})
}
