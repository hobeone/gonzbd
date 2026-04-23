// Package app wires the download pipeline: queue → downloader → decoder →
// assembler. It owns the lifecycle of each subsystem (Start, Shutdown) and
// bridges between them via a pipeline goroutine that decodes raw NNTP bodies
// and hands decoded parts to the assembler for pwrite-based out-of-order
// assembly.
//
// The cache package is constructed but deliberately not in the hot path at
// this stage; Phase 5's direct-unpack integration will thread it through the
// pipeline as an admission buffer. For Step 4.1 the cache is available for
// external callers that want to stash articles but is not exercised by the
// default flow.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/assembler"
	"github.com/hobeone/sabnzbd-go/internal/cache"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/downloader"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/postproc"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// ErrAlreadyStarted is returned by Start on the second call to a live
// Application.
var ErrAlreadyStarted = errors.New("app: already started")

// defaultCheckpointInterval is the fallback when Config.CheckpointInterval
// is zero. 30 s is the value proposed in the state-machine hardening plan
// (docs/state_machine_hardening_plan.md §B.4 "Open decisions").
//
// TODO: wire this through the top-level config.Config once the full
// config integration is in scope (Phase 4+).
const defaultCheckpointInterval = 30 * time.Second

// Config is the minimal configuration required to construct an Application.
// It is a hand-picked subset of the full config.Config surface area; over
// time Phase 4+ will replace it with a direct *config.Config reference.
type Config struct {
	// DownloadDir is the root directory where incomplete files land.
	DownloadDir string

	// CompleteDir is the root directory where completed files land.
	CompleteDir string

	// AdminDir is used for cache disk spill and other per-job transient
	// state. Must exist or be creatable.
	AdminDir string

	// CacheLimit is the memory budget for the article cache in bytes.
	// 0 disables the in-memory cache (every Save goes to disk).
	CacheLimit int64

	// Servers lists the upstream NNTP servers, in fallback order.
	// At least one entry is required.
	Servers []config.ServerConfig

	// Categories lists the configured categories.
	Categories []config.CategoryConfig

	// CheckpointInterval is how often the queue is persisted to disk
	// during an active download. A value of 0 uses defaultCheckpointInterval
	// (30 s). The ticker only fires a Save when the queue is dirty
	// (i.e. has unsaved article/file mutations), so idle queues incur
	// no I/O. Clean shutdowns always call Save regardless of this setting.
	CheckpointInterval time.Duration
}

// FileComplete is emitted on Application.FileComplete() when the assembler
// finishes writing every expected part of a file.
type FileComplete struct {
	JobID   string
	FileIdx int
}

// JobComplete is emitted on Application.JobComplete() when all files in a
// job have been successfully assembled and the job is moving to the
// PostProcessor.
type JobComplete struct {
	JobID string
}

// PostProcComplete is emitted on Application.PostProcComplete() when all
// post-processing stages for a job have finished.
type PostProcComplete struct {
	JobID string
}

// Application is the wired pipeline. Construct with New, start with Start,
// shut down with Shutdown. Public methods are safe for concurrent use after
// Start returns.
type Application struct {
	log *slog.Logger
	mu  sync.Mutex
	cfg Config

	queue            *queue.Queue
	historyRepo      *history.Repository
	cache            *cache.Cache
	downloader       *downloader.Downloader
	assembler        *assembler.Assembler
	postProcessor    *postproc.PostProcessor
	pipeline         *pipeline
	fileComplete     chan FileComplete
	jobComplete      chan JobComplete
	postProcComplete chan PostProcComplete

	// internalFileComplete ensures we never miss a queue update due to
	// a full buffer. The watchCompletions goroutine drains it.
	internalFileComplete chan FileComplete

	wg     sync.WaitGroup
	ctx    context.Context //nolint:containedctx // lifecycle context held for Shutdown
	cancel context.CancelFunc

	started atomic.Bool
	stopped atomic.Bool

	// customStages, when non-nil, overrides the default repair/unpack/finalize
	// stage sequence. Set via WithPostProcStages. Intended for tests that want
	// deterministic post-processing without invoking par2/unrar on synthetic
	// article bodies.
	customStages []postproc.Stage
}

// New constructs an Application from cfg. It does not open sockets or start
// goroutines; call Start to bring subsystems up. Returns an error when the
// config is structurally invalid (no servers, empty DownloadDir).
func New(cfg Config, repo *history.Repository, opts ...func(*Application)) (*Application, error) {
	if len(cfg.Servers) == 0 {
		return nil, errors.New("app: at least one server is required")
	}
	if cfg.DownloadDir == "" {
		return nil, errors.New("app: DownloadDir is required")
	}
	if cfg.CompleteDir == "" {
		return nil, errors.New("app: CompleteDir is required")
	}

	app := &Application{cfg: cfg, historyRepo: repo, log: slog.Default()}
	for _, o := range opts {
		o(app)
	}
	app.log = app.log.With("component", "app")
	log := app.log

	queueStateDir := filepath.Join(cfg.AdminDir, "queue")
	q, err := queue.Load(queueStateDir)
	if err != nil {
		return nil, fmt.Errorf("app: load queue: %w", err)
	}
	c := cache.New(cache.Options{Limit: cfg.CacheLimit})

	servers := make([]*downloader.Server, len(cfg.Servers))
	for i, sc := range cfg.Servers {
		servers[i] = downloader.NewServer(sc)
	}
	d := downloader.New(q, servers, downloader.Options{
		OnJobHopeless: func(jobID string) {
			job, err := q.Get(jobID)
			if err != nil {
				return
			}
			app.maybeFinalize(job, "Aborted: Too many articles failed, job is beyond repair")
		},
	}, log)

	fileComplete := make(chan FileComplete, 64)
	jobComplete := make(chan JobComplete, 16)
	postProcComplete := make(chan PostProcComplete, 16)
	internalFileComplete := make(chan FileComplete, 64)

	p := &pipeline{
		log:         log.With("component", "pipeline"),
		queue:       q,
		completions: d.Completions(),
		downloadDir: cfg.DownloadDir,
		updateCh:    make(chan (<-chan *downloader.ArticleResult), 1),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}

	stages := app.customStages
	if stages == nil {
		stages = []postproc.Stage{
			postproc.NewRepairStage(),
			postproc.NewUnpackStage(),
			postproc.NewFinalizeStage(),
		}
	}
	pp := postproc.New(postproc.Options{
		Stages: stages,
		StatusUpdater: func(jobID string, status constants.Status) {
			_ = q.SetStatus(jobID, status)
		},
		OnJobDone: func(job *postproc.Job) {
			// 1. Record in history
			stageLogJSON, _ := json.Marshal(job.StageLog)

			// Calculate download duration and server stats string
			var downloadDuration int64
			if !job.Queue.DownloadStarted.IsZero() {
				downloadDuration = int64(time.Since(job.Queue.DownloadStarted).Seconds())
			}
			if downloadDuration == 0 {
				downloadDuration = 1 // avoid div by zero
			}

			var serverStatsParts []string
			for s, b := range job.Queue.ServerStats {
				serverStatsParts = append(serverStatsParts, fmt.Sprintf("%s=%.1f MB", s, float64(b)/(1024*1024)))
			}
			serverStats := strings.Join(serverStatsParts, ", ")

			// Age of the post in days
			ageDays := 0
			if !job.Queue.AvgAge.IsZero() {
				ageDays = int(time.Since(job.Queue.AvgAge).Hours() / 24)
			}

			// Build repair summary from StageLog
			repairSummary := ""
			for _, entry := range job.StageLog {
				if entry.Stage == "repair" {
					if entry.Err != nil {
						repairSummary = fmt.Sprintf("Repair failed: %v", entry.Err)
					} else {
						repairSummary = "Repair OK"
						// If we have detailed lines (from par2 output), we could extract more,
						// but "Repair OK" or the error is a good start.
						if len(entry.Lines) > 0 {
							// Just take the first line as a summary if available
							repairSummary = entry.Lines[0]
						}
					}
					break
				}
			}
			if repairSummary == "" {
				repairSummary = "No repair needed"
			}

			entry := history.Entry{
				Completed:    time.Now(),
				Name:         job.Queue.Name,
				NzbName:      job.Queue.Filename,
				Category:     job.Queue.Category,
				Status:       "Completed",
				NzoID:        job.Queue.ID,
				Path:         job.FinalDir,
				DownloadTime: downloadDuration,
				StageLog:     string(stageLogJSON),
				Bytes:        job.Queue.TotalBytes,
				TimeAdded:    job.Queue.Added,
				Storage:      fmt.Sprintf("%dd", ageDays),
				URLInfo:      repairSummary,
				Meta:         serverStats,
			}

			if job.ParError || job.UnpackError || job.FailMsg != "" {
				entry.Status = "Failed"
				entry.FailMessage = job.FailMsg
				// Use DownloadDir for failed jobs because FinalizeStage skips the
				// move to FinalDir (the "complete" folder) on failure.
				entry.Path = job.DownloadDir
			}

			if app.historyRepo != nil {
				if err := app.historyRepo.Add(context.Background(), entry); err != nil {
					log.Warn("failed to add history entry", "job", job.Queue.ID, "err", err)
				}
			}

			// 2. Save job state one last time to ensure retry works
			jobPath := filepath.Join(app.cfg.AdminDir, "queue", "jobs", job.Queue.ID+".json.gz")
			if err := queue.SaveJob(jobPath, job.Queue); err != nil {
				log.Warn("failed to save final job state", "job", job.Queue.ID, "err", err)
			}

			// 3. Remove from queue
			if err := q.Remove(job.Queue.ID); err != nil {
				log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
			}

			// 4. Notify external subscribers
			select {
			case postProcComplete <- PostProcComplete{JobID: job.Queue.ID}:
			default:
			}
		},
		Logger: log,
	})

	asm := assembler.New(assembler.Options{
		FileInfo:          p.resolveFileInfo,
		MarkArticlesDone:   q.MarkArticlesDone,
		MarkArticlesFailed: q.MarkArticlesFailed,
		OnFileComplete: func(jobID string, fileIdx int) {
			fc := FileComplete{JobID: jobID, FileIdx: fileIdx}
			// 1. External subscribers (best-effort, non-blocking)
			select {
			case fileComplete <- fc:
			default:
			}
			// 2. Internal state update (blocking send guarantees zero dropped events)
			internalFileComplete <- fc
		},
	}, log)
	p.assembler = asm

	app.queue = q
	app.cache = c
	app.downloader = d
	app.assembler = asm
	app.postProcessor = pp
	app.pipeline = p
	app.fileComplete = fileComplete
	app.jobComplete = jobComplete
	app.postProcComplete = postProcComplete
	app.internalFileComplete = internalFileComplete
	return app, nil
}

// WithLogger sets the logger for the Application and all its subsystems.
func WithLogger(log *slog.Logger) func(*Application) {
	return func(a *Application) { a.log = log }
}

// WithPostProcStages overrides the default repair/unpack/finalize stage
// sequence with the provided stages. Intended for tests that want
// deterministic post-processing without invoking par2/unrar on synthetic
// article bodies. Passing nil is a no-op (defaults apply).
func WithPostProcStages(stages []postproc.Stage) func(*Application) {
	return func(a *Application) { a.customStages = stages }
}

// Queue returns the queue singleton. Callers add jobs via Queue().Add(job).
func (app *Application) Queue() *queue.Queue { return app.queue }

// Cache returns the article cache. Exposed for future direct-unpack wiring.
func (app *Application) Cache() *cache.Cache { return app.cache }

// FileComplete returns the receive-only channel carrying per-file
// completion notifications from the assembler. Buffered; consumers that
// drain slowly will miss events (non-blocking sends are dropped).
func (app *Application) FileComplete() <-chan FileComplete { return app.fileComplete }

// JobComplete returns the receive-only channel carrying per-job
// completion notifications. A job is complete when all its files have
// finished reassembly.
func (app *Application) JobComplete() <-chan JobComplete { return app.jobComplete }

// PostProcComplete returns the receive-only channel carrying notifications
// when a job's post-processing pipeline has finished.
func (app *Application) PostProcComplete() <-chan PostProcComplete { return app.postProcComplete }

// PausePostProcessor halts the post-processing worker after it finishes
// its current job. New completions will be queued but not processed until
// ResumePostProcessor is called.
func (app *Application) PausePostProcessor() {
	app.postProcessor.Pause()
}

// ResumePostProcessor continues processing queued jobs in the
// post-processing pipeline.
func (app *Application) ResumePostProcessor() {
	app.postProcessor.Resume()
}

// Start brings up the assembler, downloader, and pipeline goroutine. The
// given context is held for the lifetime of the Application; cancelling it
// is equivalent to calling Shutdown.
func (app *Application) Start(ctx context.Context) error {
	if !app.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	app.ctx, app.cancel = context.WithCancel(ctx)

	if err := app.assembler.Start(app.ctx); err != nil {
		return fmt.Errorf("app: start assembler: %w", err)
	}
	if err := app.downloader.Start(app.ctx); err != nil {
		return fmt.Errorf("app: start downloader: %w", err)
	}
	if err := app.postProcessor.Start(app.ctx); err != nil {
		return fmt.Errorf("app: start postprocessor: %w", err)
	}

	app.wg.Go(func() {
		app.pipeline.run(app.ctx)
	})

	app.wg.Go(func() {
		app.watchCompletions(app.ctx)
	})

	interval := app.cfg.CheckpointInterval
	if interval <= 0 {
		interval = defaultCheckpointInterval
	}
	app.wg.Go(func() {
		app.runCheckpoint(app.ctx, interval)
	})

	app.log.Info("application started", "download_dir", app.cfg.DownloadDir, "checkpoint_interval", interval)

	// Scan for and enqueue any jobs that were already complete (e.g. from
	// a previous run or a retry).
	//
	// Jobs with PostProc=true are the crash-recovery case: the previous
	// run handed them off to the post-processor but the process died
	// before OnJobDone's history.Add + queue.Remove finished. Routing
	// them through maybeFinalize would hit SetPostProcStarted, see the
	// flag already true, and silently skip — stranding the job forever.
	// Instead we bypass the CAS and hand them straight to the
	// post-processor, gated on Has() so we don't double-enqueue if
	// something else already pushed the same job in this run.
	//
	// The CAS bypass is safe because no other goroutine in this process
	// can race us: a successful Start is single-entry (ErrAlreadyStarted
	// guards re-entry), and the downloader/assembler cannot resurface a
	// completed job into PostProc territory.
	for _, job := range app.queue.List() {
		if !job.IsComplete() {
			continue
		}
		failMsg := failMsgForJob(job)
		if job.PostProc {
			if app.postProcessor.Has(job.ID) {
				continue
			}
			app.enqueuePostProc(job, failMsg)
			continue
		}
		app.maybeFinalize(job, failMsg)
	}

	return nil
}

func failMsgForJob(job *queue.Job) string {
	if job.FailedBytes > job.Par2Bytes {
		return "Aborted: Too many articles failed, job is beyond repair"
	}
	return ""
}

// maybeFinalize is the single funnel every caller goes through to hand a
// completed job to the post-processor during normal operation. It CASes
// the PostProc flag via SetPostProcStarted; the first caller wins and
// drives the handoff, subsequent callers (duplicate completion paths)
// observe the flag already set and return silently.
//
// Does NOT finalise jobs whose PostProc flag was already persisted —
// that case is crash recovery and is handled by Start's rescan, which
// calls enqueuePostProc directly.
func (app *Application) maybeFinalize(job *queue.Job, failMsg string) {
	started, err := app.queue.SetPostProcStarted(job.ID)
	if err != nil {
		app.log.Warn("failed to mark post-proc started", "job", job.ID, "err", err)
		return
	}
	if !started {
		app.log.Debug("job already in post-processing, skipping duplicate handoff", "job", job.ID, "name", job.Name)
		return
	}
	app.enqueuePostProc(job, failMsg)
}

// enqueuePostProc builds the postproc.Job and hands it to the
// PostProcessor. Callers must have ensured exclusive handoff — either by
// a successful SetPostProcStarted CAS (the normal path via
// maybeFinalize) or by checking postProcessor.Has (the Start rescan
// crash-recovery path).
func (app *Application) enqueuePostProc(job *queue.Job, failMsg string) {
	app.log.Info("sending job to post-processor", "job", job.ID, "name", job.Name, "fail_msg", failMsg)

	// Determine FinalDir based on Category
	catDir := ""
	for _, cat := range app.cfg.Categories {
		if cat.Name == job.Category {
			catDir = cat.Dir
			break
		}
	}
	finalDir := filepath.Join(app.cfg.CompleteDir, catDir, job.Name)

	ppJob := &postproc.Job{
		Queue:       job,
		DownloadDir: filepath.Join(app.cfg.DownloadDir, job.Name),
		FinalDir:    finalDir,
		FailMsg:     failMsg,
	}
	app.postProcessor.Process(ppJob)

	// Notify external subscribers
	select {
	case app.jobComplete <- JobComplete{JobID: job.ID}:
	default:
	}
}

// Shutdown cancels the lifecycle context, drains the pipeline, and stops
// every subsystem in order. Safe to call multiple times and before Start.
//
// Order matters: downloader stops first so the Completions channel closes
// and the pipeline goroutine drains. The assembler stops after the pipeline
// exits so any WriteArticle calls in-flight complete. Finally the cache is
// flushed to preserve any in-memory articles across a restart.
func (app *Application) Shutdown() error {
	if !app.started.Load() {
		return nil
	}
	if !app.stopped.CompareAndSwap(false, true) {
		return nil
	}

	// Cancel first so any ctx-aware operations (WriteArticle, WaitN on the
	// rate limiter) observe the shutdown signal immediately.
	app.cancel()

	// Stop order: downloader → pipeline (auto-drains on closed channel) →
	// assembler → cache flush.
	var firstErr error
	if err := app.downloader.Stop(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("app: stop downloader: %w", err)
	}
	if err := app.postProcessor.Stop(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("app: stop postprocessor: %w", err)
	}
	app.wg.Wait()
	if err := app.assembler.Stop(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("app: stop assembler: %w", err)
	}
	if err := app.cache.Flush(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("app: flush cache: %w", err)
	}
	if err := app.queue.Save(filepath.Join(app.cfg.AdminDir, "queue")); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("app: save queue: %w", err)
	}
	return firstErr
}

// RetryHistoryJob moves a job from the history back to the active queue.
// It loads the job state from disk, resets its status, and re-adds it.
func (app *Application) RetryHistoryJob(ctx context.Context, jobID string) error {
	// 1. Ensure the job is in history
	_, err := app.historyRepo.Get(ctx, jobID)
	if err != nil {
		return fmt.Errorf("app: job not in history: %w", err)
	}

	// 2. Load the job state from disk
	jobPath := filepath.Join(app.cfg.AdminDir, "queue", "jobs", jobID+".json.gz")
	job, err := queue.LoadJob(jobPath)
	if err != nil {
		return fmt.Errorf("app: load job state: %w", err)
	}

	// 3. Reset status and re-add to queue
	job.Status = constants.StatusQueued
	job.PostProc = false

	// Reset transient download bookkeeping so post-proc duration and
	// per-server byte counters start fresh on the retried attempt;
	// otherwise OnJobDone's duration math uses the original start
	// timestamp and history shows bogus stats.
	job.DownloadStarted = time.Time{}
	job.ServerStats = nil

	// Reset failed articles so they can be tried again
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
		return fmt.Errorf("app: add to queue: %w", err)
	}

	// 4. Remove from history
	if _, err := app.historyRepo.Delete(ctx, jobID); err != nil {
		// Log but don't fail; the job is already back in the queue
		app.log.Warn("failed to delete history entry after retry", "job", jobID, "err", err)
	}

	// 5. Trigger post-processing if already complete (which it should be)
	if job.IsComplete() {
		app.maybeFinalize(job, failMsgForJob(job))
	}

	app.log.Info("job retried from history", "job", jobID, "name", job.Name)
	return nil
	}

// ReloadDownloader stops the current downloader and starts a new one with
// updated server configurations. It re-plumbs the pipeline to use the new
// downloader's completion channel.
func (app *Application) ReloadDownloader(scs []config.ServerConfig) error {
	app.mu.Lock()
	defer app.mu.Unlock()

	if !app.started.Load() || app.stopped.Load() {
		return errors.New("app: cannot reload downloader (not running)")
	}

	// 1. Stop the current downloader.
	if err := app.downloader.Stop(); err != nil {
		return fmt.Errorf("stop old downloader: %w", err)
	}

	// 2. Initialize the new downloader.
	servers := make([]*downloader.Server, len(scs))
	for i, sc := range scs {
		servers[i] = downloader.NewServer(sc)
	}
	newDownloader := downloader.New(app.queue, servers, downloader.Options{}, app.log)

	// 3. Start the new downloader.
	if err := newDownloader.Start(app.ctx); err != nil {
		return fmt.Errorf("start new downloader: %w", err)
	}

	// 4. Swap references and re-plumb.
	app.downloader = newDownloader
	app.pipeline.setCompletions(newDownloader.Completions())

	return nil
}

// runCheckpoint is a goroutine that periodically persists the queue to disk.
// It fires every interval, but only calls Save when the queue is dirty
// (i.e. has unsaved article/file state mutations). The goroutine exits cleanly
// when ctx is cancelled (the same signal Shutdown sends).
//
// Coalescing with the Shutdown save: Shutdown calls app.cancel() before
// wg.Wait(), so this goroutine exits before the post-wg.Wait Save in
// Shutdown runs. The Shutdown Save is therefore always the last write and
// captures any final mutations that landed after the last tick.
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
			dir := filepath.Join(app.cfg.AdminDir, "queue")
			if err := app.queue.Save(dir); err != nil {
				app.log.Error("periodic queue checkpoint failed", "err", err)
			} else {
				app.log.Debug("periodic queue checkpoint saved")
			}
		}
	}
}

// watchCompletions is a dedicated goroutine that updates the queue state
// when files complete. It uses a blocking channel read to ensure zero events
// are dropped.
func (app *Application) watchCompletions(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case fc := <-app.internalFileComplete:
			if err := app.queue.MarkFileComplete(fc.JobID, fc.FileIdx); err != nil {
				app.log.Warn("failed to mark file complete", "job", fc.JobID, "fileidx", fc.FileIdx, "err", err)
				continue
			}
			app.log.Info("file marked complete in queue", "job", fc.JobID, "fileidx", fc.FileIdx)

			// Check if the whole job is now complete.
			job, err := app.queue.Get(fc.JobID)
			if err != nil {
				app.log.Warn("job not found while checking for completion", "job", fc.JobID, "err", err)
				continue
			}

			if job.IsComplete() {
				app.maybeFinalize(job, failMsgForJob(job))
			}
		}
	}
}
