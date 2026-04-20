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
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/assembler"
	"github.com/hobeone/sabnzbd-go/internal/cache"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/downloader"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/postproc"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// ErrAlreadyStarted is returned by Start on the second call to a live
// Application.
var ErrAlreadyStarted = errors.New("app: already started")

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
	d := downloader.New(q, servers, downloader.Options{}, log)

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

	pp := postproc.New(postproc.Options{
		Stages: []postproc.Stage{
			postproc.NewRepairStage(),
			postproc.NewUnpackStage(),
			postproc.NewFinalizeStage(),
		},
		OnJobDone: func(job *postproc.Job) {
			// 1. Record in history
			stageLogJSON, _ := json.Marshal(job.StageLog)
			entry := history.Entry{
				Completed:    time.Now(),
				Name:         job.Queue.Name,
				NzbName:      job.Queue.Filename,
				Category:     job.Queue.Category,
				Status:       "Completed",
				NzoID:        job.Queue.ID,
				Path:         job.FinalDir,
				DownloadTime: int64(job.Queue.TotalBytes), // placeholder
				StageLog:     string(stageLogJSON),
				Bytes:        job.Queue.TotalBytes,
				TimeAdded:    time.Now(), // TODO: use actual time added
			}
			if job.ParError || job.UnpackError || job.FailMsg != "" {
				entry.Status = "Failed"
				entry.FailMessage = job.FailMsg
			}

			if app.historyRepo != nil {
				if err := app.historyRepo.Add(context.Background(), entry); err != nil {
					log.Warn("failed to add history entry", "job", job.Queue.ID, "err", err)
				}
			}

			// 2. Remove from queue
			if err := q.Remove(job.Queue.ID); err != nil {
				log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
			}

			// 3. Notify external subscribers
			select {
			case postProcComplete <- PostProcComplete{JobID: job.Queue.ID}:
			default:
			}
		},
		Logger: log,
	})

	asm := assembler.New(assembler.Options{
		FileInfo: p.resolveFileInfo,
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

	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		app.pipeline.run(app.ctx)
	}()

	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		app.watchCompletions(app.ctx)
	}()

	app.log.Info("application started", "download_dir", app.cfg.DownloadDir)
	return nil
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
				app.log.Info("job complete, sending to post-processor", "job", job.ID, "name", job.Name)

				// 1. Trigger PostProcessor
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
				}
				app.postProcessor.Process(ppJob)

				// 2. Notify external subscribers
				select {
				case app.jobComplete <- JobComplete{JobID: job.ID}:
				default:
				}
			}
		}
	}
}
