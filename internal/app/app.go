// Package app wires the download pipeline: queue → downloader → decoder →
// assembler. It owns the lifecycle of each subsystem (Start, Shutdown) and
// bridges between them via a pipeline goroutine that decodes raw NNTP bodies
// and hands decoded parts to the assembler for pwrite-based out-of-order
// assembly.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/assembler"
	"github.com/hobeone/sabnzbd-go/internal/bpsmeter"
	"github.com/hobeone/sabnzbd-go/internal/cache"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/downloader"
	"github.com/hobeone/sabnzbd-go/internal/fsutil"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/postproc"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// ErrAlreadyStarted is returned by Start on the second call to a live
// Application.
var ErrAlreadyStarted = errors.New("app: already started")

const defaultCheckpointInterval = 30 * time.Second

// Config is the minimal configuration required to construct an Application.
type Config struct {
	DownloadDir        string
	CompleteDir        string
	AdminDir           string
	CacheLimit         int64
	Servers            []config.ServerConfig
	Categories         []config.CategoryConfig
	CheckpointInterval time.Duration
	Sanitize           fsutil.SanitizeOptions
}

// FileComplete is emitted on Application.FileComplete() when a file is done.
type FileComplete struct {
	JobID   string
	FileIdx int
}

// JobComplete is emitted when all files in a job are assembled.
type JobComplete struct {
	JobID string
}

// PostProcComplete is emitted when post-processing finished.
type PostProcComplete struct {
	JobID string
}

// EventEmitter defines the interface for broadcasting real-time events.
type EventEmitter interface {
	Broadcast(event Event)
}

// Event represents a real-time notification sent to the UI.
type Event struct {
	Type      string `json:"event"`
	Speed     int64  `json:"speed,omitempty"`
	Remaining int64  `json:"remaining,omitempty"`
}

type dummyEmitter struct{}

func (d dummyEmitter) Broadcast(_ Event) {
}

// Application manages the download and post-processing pipeline.
type Application struct {
	log     *slog.Logger
	mu      sync.Mutex
	cfg     Config
	emitter EventEmitter
	meter   *bpsmeter.Meter

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

	internalFileComplete chan FileComplete

	wg     sync.WaitGroup
	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	started atomic.Bool
	stopped atomic.Bool

	customStages []postproc.Stage
}

// SetEmitter injects a broadcaster for real-time events.
func (app *Application) SetEmitter(e EventEmitter) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if e == nil {
		app.emitter = dummyEmitter{}
		return
	}
	app.emitter = e
}

// New constructs an Application from cfg.
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

	app := &Application{
		cfg:                  cfg,
		historyRepo:          repo,
		emitter:              dummyEmitter{},
		fileComplete:         make(chan FileComplete, 16),
		internalFileComplete: make(chan FileComplete, 128),
		jobComplete:          make(chan JobComplete, 8),
		postProcComplete:     make(chan PostProcComplete, 8),
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

	queueStateDir := filepath.Join(cfg.AdminDir, "queue")
	q, err := queue.Load(queueStateDir)
	if err != nil {
		return nil, fmt.Errorf("app: load queue: %w", err)
	}
	app.queue = q

	c := cache.New(cache.Options{Limit: cfg.CacheLimit})
	app.cache = c

	servers := make([]*downloader.Server, len(cfg.Servers))
	for i, sc := range cfg.Servers {
		servers[i] = downloader.NewServer(sc)
	}
	d := downloader.New(q, servers, app.meter, downloader.Options{
		OnJobHopeless: func(jobID string) {
			job, err := q.Get(jobID)
			if err != nil {
				return
			}
			app.maybeFinalize(job, "Aborted: Too many articles failed, job is beyond repair")
		},
	}, log)
	app.downloader = d

	p := &pipeline{
		log:         log.With("component", "pipeline"),
		queue:       q,
		completions: d.Completions(),
		downloadDir: cfg.DownloadDir,
		sanitize:    cfg.Sanitize,
		updateCh:    make(chan (<-chan *downloader.ArticleResult), 1),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}
	app.pipeline = p

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
			stageLogJSON, _ := json.Marshal(job.StageLog)
			var downloadDuration int64
			if !job.Queue.DownloadStarted.IsZero() {
				downloadDuration = int64(time.Since(job.Queue.DownloadStarted).Seconds())
			}
			if downloadDuration == 0 {
				downloadDuration = 1
			}
			var serverStatsParts []string
			for s, b := range job.Queue.ServerStats {
				serverStatsParts = append(serverStatsParts, fmt.Sprintf("%s=%.1f MB", s, float64(b)/(1024*1024)))
			}
			serverStats := strings.Join(serverStatsParts, ", ")
			ageDays := 0
			if !job.Queue.AvgAge.IsZero() {
				ageDays = int(time.Since(job.Queue.AvgAge).Hours() / 24)
			}
			repairSummary := ""
			for _, entry := range job.StageLog {
				if entry.Stage == "repair" {
					if entry.Err != nil {
						repairSummary = fmt.Sprintf("Repair failed: %v", entry.Err)
					} else {
						repairSummary = "Repair OK"
						if len(entry.Lines) > 0 {
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
				entry.Path = job.DownloadDir
			}
			if app.historyRepo != nil {
				if err := app.historyRepo.Add(context.Background(), entry); err != nil {
					log.Warn("failed to add history entry", "job", job.Queue.ID, "err", err)
				}
			}
			jobPath := filepath.Join(app.cfg.AdminDir, "queue", "jobs", job.Queue.ID+".json.gz")
			if err := queue.SaveJob(jobPath, job.Queue); err != nil {
				log.Warn("failed to save final job state", "job", job.Queue.ID, "err", err)
			}
			if err := q.Remove(job.Queue.ID); err != nil {
				log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
			}
			select {
			case app.postProcComplete <- PostProcComplete{JobID: job.Queue.ID}:
			default:
			}
			app.emitter.Broadcast(Event{Type: "history_updated"})
		},
		Logger: log,
	})
	app.postProcessor = pp

	asm := assembler.New(assembler.Options{
		FileInfo:           p.resolveFileInfo,
		MarkArticlesDone:   q.MarkArticlesDone,
		MarkArticlesFailed: q.MarkArticlesFailed,
		OnFileComplete: func(jobID string, fileIdx int) {
			fc := FileComplete{JobID: jobID, FileIdx: fileIdx}
			select {
			case app.fileComplete <- fc:
			default:
			}
			app.internalFileComplete <- fc
		},
	}, log)
	app.assembler = asm
	p.assembler = asm

	return app, nil
}

// Queue returns the application's download queue.
func (app *Application) Queue() *queue.Queue { return app.queue }

// AddJob validates, deduplicates, and enqueues a new download job. If force
// is false and a duplicate is detected, the job is added in a paused state.
func (app *Application) AddJob(ctx context.Context, job *queue.Job, rawNZB []byte, force bool) error {
	nzbDir := filepath.Join(app.cfg.AdminDir, "nzb")
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
		if _, err := os.Stat(filepath.Join(nzbDir, job.Filename)); err == nil {
			isDuplicate = true
			dupReason = "found in admin/nzb/ backup dir (filename)"
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
	baseName := job.Name
	for i := 1; ; i++ {
		collision := false
		if app.queue.ExistsByName(job.Name) {
			collision = true
		}
		if !collision {
			if _, err := os.Stat(filepath.Join(app.cfg.DownloadDir, job.Name)); err == nil {
				collision = true
			}
		}
		if !collision {
			if _, err := os.Stat(filepath.Join(app.cfg.CompleteDir, job.Name)); err == nil {
				collision = true
			}
			if !collision {
				for _, cat := range app.cfg.Categories {
					if cat.Dir == "" {
						continue
					}
					if _, err := os.Stat(filepath.Join(app.cfg.CompleteDir, cat.Dir, job.Name)); err == nil {
						collision = true
						break
					}
				}
			}
		}
		if !collision {
			break
		}
		job.Name = fmt.Sprintf("%s.%d", baseName, i)
	}
	if !isDuplicate && job.Filename != "" {
		backupPath := filepath.Join(nzbDir, job.Filename)
		_ = os.WriteFile(backupPath, rawNZB, 0o600)
	}
	if err := app.queue.Add(job); err != nil {
		return fmt.Errorf("app: add to queue: %w", err)
	}
	app.emitter.Broadcast(Event{Type: "queue_updated"})
	app.log.Info("job added", "name", job.Name, "id", job.ID, "status", job.Status)
	return nil
}

// RemoveJob cancels and removes a job from the queue, deleting its download directory.
func (app *Application) RemoveJob(id string) error {
	job, err := app.queue.Get(id)
	if err != nil {
		return err
	}
	path := filepath.Join(app.cfg.DownloadDir, job.Name)
	_ = os.RemoveAll(path)
	if err := app.queue.Remove(id); err != nil {
		return err
	}
	app.emitter.Broadcast(Event{Type: "queue_updated"})
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
		_ = os.RemoveAll(entry.Path)
	}
	if _, err := app.historyRepo.Delete(ctx, id); err != nil {
		return err
	}
	app.emitter.Broadcast(Event{Type: "history_updated"})
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
func (app *Application) Start(ctx context.Context) error {
	if !app.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	app.ctx, app.cancel = context.WithCancel(ctx)
	if err := app.assembler.Start(app.ctx); err != nil {
		return err
	}
	if err := app.downloader.Start(app.ctx); err != nil {
		return err
	}
	if err := app.postProcessor.Start(app.ctx); err != nil {
		return err
	}
	app.wg.Go(func() { app.pipeline.run(app.ctx) })
	app.wg.Go(func() { app.watchCompletions(app.ctx) })
	interval := app.cfg.CheckpointInterval
	if interval <= 0 {
		interval = defaultCheckpointInterval
	}
	app.wg.Go(func() { app.runCheckpoint(app.ctx, interval) })
	app.wg.Go(func() { app.runMetricsPush(app.ctx) })
	app.log.Info("application started")

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

func (app *Application) runMetricsPush(ctx context.Context) {
	ticker := time.NewTicker(1000 * time.Millisecond)
	defer ticker.Stop()
	var lastRemaining int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			remaining := app.queue.TotalRemainingBytes()
			if remaining > 0 || lastRemaining > 0 {
				app.emitter.Broadcast(Event{
					Type:      "metrics",
					Speed:     int64(app.downloader.Speed()),
					Remaining: remaining,
				})
				// Trigger a table refresh to update individual job percentages
				app.emitter.Broadcast(Event{Type: "queue_updated"})
			}
			lastRemaining = remaining
		}
	}
}

// Shutdown stops the downloader, post-processor, and assembler, flushes the
// cache, and persists the queue to disk. Safe to call multiple times.
func (app *Application) Shutdown() error {
	if !app.started.Load() || !app.stopped.CompareAndSwap(false, true) {
		return nil
	}
	app.cancel()
	_ = app.downloader.Stop()
	_ = app.postProcessor.Stop()
	app.wg.Wait()
	_ = app.assembler.Stop()
	_ = app.cache.Flush()
	_ = app.queue.Save(filepath.Join(app.cfg.AdminDir, "queue"))
	return nil
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
			_ = app.queue.Save(filepath.Join(app.cfg.AdminDir, "queue"))
		}
	}
}

func (app *Application) watchCompletions(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case fc := <-app.internalFileComplete:
			if err := app.queue.MarkFileComplete(fc.JobID, fc.FileIdx); err != nil {
				continue
			}
			app.emitter.Broadcast(Event{Type: "queue_updated"})
			job, err := app.queue.Get(fc.JobID)
			if err == nil && job.IsComplete() {
				app.maybeFinalize(job, failMsgForJob(job))
			}
		}
	}
}

func (app *Application) maybeFinalize(job *queue.Job, failMsg string) {
	started, err := app.queue.SetPostProcStarted(job.ID)
	if err == nil && started {
		app.enqueuePostProc(job, failMsg)
	}
}

func (app *Application) enqueuePostProc(job *queue.Job, failMsg string) {
	catDir := ""
	for _, cat := range app.cfg.Categories {
		if cat.Name == job.Category {
			catDir = cat.Dir
			break
		}
	}
	app.postProcessor.Process(&postproc.Job{
		Queue:       job,
		DownloadDir: filepath.Join(app.cfg.DownloadDir, job.Name),
		FinalDir:    filepath.Join(app.cfg.CompleteDir, catDir, job.Name),
		Sanitize:    app.cfg.Sanitize,
		FailMsg:     failMsg,
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

// RetryHistoryJob re-enqueues a completed/failed history job for re-download.
// Failed articles are reset; the history entry is deleted on success.
func (app *Application) RetryHistoryJob(ctx context.Context, jobID string) error {
	_, err := app.historyRepo.Get(ctx, jobID)
	if err != nil {
		return err
	}
	jobPath := filepath.Join(app.cfg.AdminDir, "queue", "jobs", jobID+".json.gz")
	job, err := queue.LoadJob(jobPath)
	if err != nil {
		return err
	}
	job.Status = constants.StatusQueued
	job.PostProc = false
	job.DownloadStarted = time.Time{}
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
	if job.IsComplete() {
		app.maybeFinalize(job, failMsgForJob(job))
	}
	return nil
}

// ReloadDownloader stops the current downloader and starts a new one with
// the given server configurations. Used when server settings change at runtime.
func (app *Application) ReloadDownloader(scs []config.ServerConfig) error {
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.started.Load() || app.stopped.Load() {
		return errors.New("app: not running")
	}
	_ = app.downloader.Stop()
	servers := make([]*downloader.Server, len(scs))
	for i, sc := range scs {
		servers[i] = downloader.NewServer(sc)
	}
	newDownloader := downloader.New(app.queue, servers, app.meter, downloader.Options{}, app.log)
	if err := newDownloader.Start(app.ctx); err != nil {
		return err
	}
	app.downloader = newDownloader
	app.pipeline.setCompletions(newDownloader.Completions())
	return nil
}

func WithLogger(log *slog.Logger) func(*Application) {
	return func(a *Application) { a.log = log }
}

func WithMeter(m *bpsmeter.Meter) func(*Application) {
	return func(a *Application) { a.meter = m }
}

func WithPostProcStages(stages []postproc.Stage) func(*Application) {
	return func(a *Application) { a.customStages = stages }
}

func failMsgForJob(job *queue.Job) string {
	if job.FailedBytes > job.Par2Bytes {
		return "Aborted: Too many articles failed, job is beyond repair"
	}
	return ""
}
