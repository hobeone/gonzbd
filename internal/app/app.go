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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/cache"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/unpack"
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

	// Download tuning.
	BandwidthMax     int64 // bytes/sec; 0 = unlimited
	BandwidthPerc    int   // 0-100; percentage of BandwidthMax
	MinFreeSpace     int64 // bytes; 0 = disabled
	MaxArtTries      int   // per-article retry cap across all servers
	MaxArtOpt        int   // per-article retry cap on optional servers
	TopOnly          bool  // restrict dispatch to highest-priority server
	NoPenalties      bool  // use short penalties for testing
	PreCheck         bool  // STAT before BODY
	PropagationDelay int   // minutes to hold new jobs before downloading

	// PostProc pipeline configuration.
	Sorters              []config.SorterConfig
	ScriptDir            string
	DeobfuscateFilenames bool
	EnableUnrar          bool
	Enable7zip           bool
	EnableParCleanup     bool
	EnableRarCleanup     bool
	Par2Command          string
	Par2Turbo            bool
	UnrarCommand         string
	SevenzCommand        string
	IgnoreUnrarDates     bool
	OverwriteFiles       bool
	FlatUnpack           bool

	// ScriptStage metadata injected into SAB_* env vars.
	Version    string
	APIKey     string
	ListenAddr string
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
	Type          string `json:"event"`
	Speed         int64  `json:"speed,omitempty"`
	Remaining     int64  `json:"remaining,omitempty"`
	SpeedLimit    int64  `json:"speed_limit"`
	BandwidthMax  int64  `json:"bandwidth_max"`
	BandwidthPerc int    `json:"bandwidth_perc"`
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
	notifyDispatcher *notifier.Dispatcher

	internalFileComplete chan FileComplete

	wg     sync.WaitGroup
	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	started atomic.Bool
	stopped atomic.Bool

	bandwidthMax  atomic.Int64 // configured bandwidth ceiling in bytes/sec
	bandwidthPerc atomic.Int32 // configured bandwidth percentage (1-100)

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

// SetNotifier injects a notification dispatcher for lifecycle events.
func (app *Application) SetNotifier(d *notifier.Dispatcher) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.notifyDispatcher = d
}

// New constructs an Application from cfg.
func New(cfg Config, repo *history.Repository, opts ...func(*Application)) (*Application, error) {
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
		MaxArtTries:      cfg.MaxArtTries,
		MaxArtOpt:        cfg.MaxArtOpt,
		TopOnly:          cfg.TopOnly,
		NoPenalties:      cfg.NoPenalties,
		PreCheck:         cfg.PreCheck,
		PropagationDelay: time.Duration(cfg.PropagationDelay) * time.Minute,
		OnJobHopeless: func(jobID string) {
			snap := q.SnapshotJob(jobID)
			if snap == nil {
				return
			}
			app.maybeFinalize(jobID, "Aborted: Too many articles failed, job is beyond repair")
		},
	}, log)
	app.downloader = d

	// Apply initial bandwidth limit from config.
	app.bandwidthMax.Store(cfg.BandwidthMax)
	perc := cfg.BandwidthPerc
	if perc <= 0 || perc > 100 {
		perc = 100
	}
	app.bandwidthPerc.Store(int32(perc))
	if cfg.BandwidthMax > 0 {
		d.SetSpeedLimit(cfg.BandwidthMax * int64(perc) / 100)
	}

	p := &pipeline{
		log:         log.With("component", "pipeline"),
		queue:       q,
		completions: d.Completions(),
		downloadDir: cfg.DownloadDir,
		sanitize:    cfg.Sanitize,
		updateCh:    make(chan completionSwap, 1),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}
	app.pipeline = p

	stages := app.customStages
	if stages == nil {
		var stageList []postproc.Stage

		// Repair stage: configurable par2 binary, turbo mode, and cleanup.
		stageList = append(stageList, postproc.NewRepairStageWith(
			par2.RunOptions{Command: cfg.Par2Command, Turbo: cfg.Par2Turbo},
			cfg.EnableParCleanup,
		))

		// Unpack stage: conditionally included based on enable flags.
		if cfg.EnableUnrar || cfg.Enable7zip {
			stageList = append(stageList, postproc.NewUnpackStageWith(unpack.Options{
				UnrarCommand:     cfg.UnrarCommand,
				SevenZipCommand:  cfg.SevenzCommand,
				OverwriteFiles:   cfg.OverwriteFiles,
				IgnoreUnrarDates: cfg.IgnoreUnrarDates,
				OneFolder:        cfg.FlatUnpack,
			}, cfg.EnableRarCleanup))
		}

		if cfg.DeobfuscateFilenames {
			stageList = append(stageList, postproc.NewDeobfuscateStage())
		}
		if len(cfg.Sorters) > 0 {
			rules := sorterRulesFromConfig(cfg.Sorters)
			if len(rules) > 0 {
				stageList = append(stageList, postproc.NewSortStage(rules, cfg.CompleteDir))
			}
		}
		if cfg.ScriptDir != "" {
			stageList = append(stageList, postproc.NewScriptStage(
				cfg.ScriptDir, cfg.CompleteDir,
				cfg.Version, cfg.APIKey, cfg.ListenAddr,
			))
		}
		stageList = append(stageList, postproc.NewFinalizeStage())
		stages = stageList
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
			serverStatsParts := make([]string, 0, len(job.Queue.ServerStats))
			// Sort keys for deterministic output in history entries.
			serverNames := make([]string, 0, len(job.Queue.ServerStats))
			for s := range job.Queue.ServerStats {
				serverNames = append(serverNames, s)
			}
			sort.Strings(serverNames)
			for _, s := range serverNames {
				b := job.Queue.ServerStats[s]
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
			// Save the full job payload first — RetryHistoryJob needs
			// this file to re-enqueue. If this fails, don't commit
			// to the DB or remove from queue.
			histJobsDir := filepath.Join(app.cfg.AdminDir, "history", "jobs")
			if err := os.MkdirAll(histJobsDir, 0o750); err != nil {
				log.Warn("failed to create history jobs dir", "err", err)
			}
			jobPath := filepath.Join(histJobsDir, job.Queue.ID+".json.gz")
			if err := queue.SaveJob(jobPath, job.Queue); err != nil {
				log.Error("failed to save final job state; keeping job in queue",
					"job", job.Queue.ID, "err", err)
				app.emitter.Broadcast(Event{Type: "queue_updated"})
				return
			}
			if app.historyRepo != nil {
				dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := app.historyRepo.Add(dbCtx, entry); err != nil {
					log.Error("failed to add history entry; keeping job in queue for recovery",
						"job", job.Queue.ID, "err", err)
					dbCancel()
					// Clean up the orphaned payload file.
					_ = os.Remove(jobPath)
					// Don't remove from queue — the job stays recoverable.
					app.emitter.Broadcast(Event{Type: "queue_updated"})
					return
				}
				dbCancel()
			}
			if err := q.Remove(job.Queue.ID); err != nil {
				log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
			}
			select {
			case app.postProcComplete <- PostProcComplete{JobID: job.Queue.ID}:
			default:
			}
			app.emitter.Broadcast(Event{Type: "history_updated"})

			// Fire notification event with a bounded timeout so a
			// misbehaving sink can't hang the postproc worker forever.
			if app.notifyDispatcher != nil {
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
		},
		Logger: log,
	})
	app.postProcessor = pp

	asm := assembler.New(assembler.Options{
		FileInfo:           p.resolveFileInfo,
		MarkArticlesDone:   q.MarkArticlesDone,
		MarkArticlesFailed: q.MarkArticlesFailed,
		MinFreeBytes:       cfg.MinFreeSpace,
		OnLowDisk: func(dir string, freeBytes int64) {
			app.downloader.Pause()
			app.log.Warn("low disk space, downloads paused",
				"dir", dir,
				"freeMB", freeBytes/(1024*1024))
		},
		OnFileComplete: func(jobID string, fileIdx int) {
			fc := FileComplete{JobID: jobID, FileIdx: fileIdx}
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
		if _, err := os.Stat(filepath.Join(nzbDir, filepath.Base(job.Filename))); err == nil {
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
		backupPath := filepath.Join(nzbDir, filepath.Base(job.Filename))
		_ = os.WriteFile(backupPath, rawNZB, 0o600)
	}
	addStatus := job.Status // snapshot before q.Add notifies the dispatcher
	if err := app.queue.Add(job); err != nil {
		return fmt.Errorf("app: add to queue: %w", err)
	}
	app.emitter.Broadcast(Event{Type: "queue_updated"})
	app.log.Info("job added", "name", job.Name, "id", job.ID, "status", addStatus)
	return nil
}

// RemoveJob cancels and removes a job from the queue, deleting its download directory.
func (app *Application) RemoveJob(ctx context.Context, id string, deleteFiles bool) error {
	snap := app.queue.SnapshotJob(id)
	if snap == nil {
		return fmt.Errorf("job %q not found", id)
	}
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
		path := filepath.Join(app.cfg.DownloadDir, snap.Name)
		_ = os.RemoveAll(path)
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
	app.wg.Go(func() { app.pipeline.run(app.ctx) })
	app.wg.Go(func() { app.watchCompletions(app.ctx) })
	interval := app.cfg.CheckpointInterval
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

	return nil
}

func (app *Application) runMetricsPush(ctx context.Context) {
	ticker := time.NewTicker(1000 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			remaining := app.queue.TotalRemainingBytes()
			app.emitter.Broadcast(Event{
				Type:          "metrics",
				Speed:         int64(app.downloader.Speed()),
				Remaining:     remaining,
				SpeedLimit:    app.downloader.SpeedLimit(),
				BandwidthMax:  app.bandwidthMax.Load(),
				BandwidthPerc: int(app.bandwidthPerc.Load()),
			})
			// Trigger a table refresh only while actively downloading so
			// individual job percentages update, but avoid pointless
			// refreshes when idle.
			if remaining > 0 {
				app.emitter.Broadcast(Event{Type: "queue_updated"})
			}
		}
	}
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
//  5. Stop the post-processor, flush cache, save queue.
func (app *Application) Shutdown() error {
	if !app.started.Load() || !app.stopped.CompareAndSwap(false, true) {
		return nil
	}
	_ = app.downloader.Stop()
	_ = app.assembler.Stop()
	app.cancel()
	app.wg.Wait()
	_ = app.postProcessor.Stop()
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
			// Drain any pending completions so they're applied to
			// the queue before it's saved to disk during shutdown.
			app.drainCompletions()
			return
		case fc := <-app.internalFileComplete:
			app.handleFileComplete(fc)
		}
	}
}

// handleFileComplete processes a single file completion event.
func (app *Application) handleFileComplete(fc FileComplete) {
	if err := app.queue.MarkFileComplete(fc.JobID, fc.FileIdx); err != nil {
		return
	}
	app.emitter.Broadcast(Event{Type: "queue_updated"})
	snap := app.queue.SnapshotJob(fc.JobID)
	if snap != nil && snap.IsComplete() {
		app.maybeFinalize(fc.JobID, failMsgForJob(snap))
	}
}

// drainCompletions processes all buffered events on internalFileComplete.
func (app *Application) drainCompletions() {
	for {
		select {
		case fc := <-app.internalFileComplete:
			app.handleFileComplete(fc)
		default:
			return
		}
	}
}

func (app *Application) maybeFinalize(jobID string, failMsg string) {
	started, err := app.queue.SetPostProcStarted(jobID)
	if err == nil && started {
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
	// Release cached file info for this job; the assembler no longer
	// needs it, and keeping it around leaks memory across many downloads.
	app.pipeline.forgetJob(job.ID)

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
	jobPath := filepath.Join(app.cfg.AdminDir, "history", "jobs", jobID+".json.gz")
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
	snap := app.queue.SnapshotJob(jobID)
	if snap != nil && snap.IsComplete() {
		app.maybeFinalize(jobID, failMsgForJob(snap))
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

	// Wait for the pipeline to drain all buffered results from the old
	// downloader's (now-closed) completions channel. setCompletions
	// blocks until the pipeline's run() loop receives the update, which
	// only happens after the pipeline has finished processing all
	// buffered results and detected the channel close.
	app.pipeline.setCompletions(nil)

	// Now it's safe to clear emitted: no more MarkArticleDone calls
	// from old results, so notifyCh won't be consumed between clear
	// and the new downloader's first dispatch pass.
	app.queue.ClearAllEmitted()

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

// WithNotifier injects a notification dispatcher for lifecycle events.
func WithNotifier(d *notifier.Dispatcher) func(*Application) {
	return func(a *Application) { a.notifyDispatcher = d }
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
	app.bandwidthPerc.Store(int32(perc))
}

// SetDownloadDir updates the download directory used for new jobs.
// Already-queued jobs are unaffected since their paths were computed at
// enqueue time. The caller is responsible for creating the directory.
func (app *Application) SetDownloadDir(dir string) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.cfg.DownloadDir = dir
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
	app.cfg.CompleteDir = dir
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

// ServerStatus returns a snapshot of all NNTP server connection state,
// including per-connection article activity. Returns nil when the
// downloader is not running.
func (app *Application) ServerStatus() []downloader.ServerSnapshot {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.downloader != nil {
		return app.downloader.ServerStatus()
	}
	return nil
}

func failMsgForJob(job *queue.Job) string {
	if job.FailedBytes == 0 {
		return ""
	}
	// If ALL bytes in the job failed, it's hopeless regardless of PAR2.
	if job.FailedBytes >= job.TotalBytes {
		return "Aborted: Too many articles failed, job is beyond repair"
	}
	// If PAR2 files exist and the failure exceeds repair capacity, abort.
	if job.Par2Bytes > 0 && job.FailedBytes > job.Par2Bytes {
		return "Aborted: Too many articles failed, job is beyond repair"
	}
	// Partial failure with no PAR2 — let post-processing attempt extraction.
	return ""
}
