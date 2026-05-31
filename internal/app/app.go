// Package app wires the download pipeline: queue → downloader → decoder →
// assembler. It owns the lifecycle of each subsystem (Start, Shutdown) and
// bridges between them via a pipeline goroutine that decodes raw NNTP bodies
// and hands decoded parts to the assembler for pwrite-based out-of-order
// assembly.
package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/notifier"
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
	WriteCacheBytes    int64
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
	ScriptDir            string
	DeobfuscateFilenames bool
	IgnoreSamples        bool
	EnableUnrar          bool
	Enable7zip           bool
	EnableFileJoin       bool
	EnableRecursive      bool
	EnableParCleanup     bool
	EnableRarCleanup     bool
	Par2Command          string
	Par2Turbo            bool
	UnrarCommand         string
	SevenzCommand        string
	IgnoreUnrarDates     bool
	OverwriteFiles       bool
	FlatUnpack           bool
	UseGoRAR             bool
	UseGo7z              bool
	UseGoPar2            bool
	GoRarFallback        bool
	Go7zFallback         bool
	GoPar2Fallback       bool
	// SkipQuickCheck disables the CRC pre-verification pass before par2 repair.
	// Zero value (false) preserves the existing behavior: quickcheck runs.
	SkipQuickCheck    bool
	CleanupExtensions []string
	FolderRename      bool
	Nice              string
	Ionice            string
	Permissions       string
	PasswordFile      string
	ExtraUnrarParams  string
	ExtraPar2Params   string
	ScriptCanFail     bool

	// DirectUnpack enables extraction of RAR archives while the download
	// is still in progress. Completed volumes are fed to the extractor
	// as they arrive. Falls back to standard extraction
	// on any error. Requires EnableUnrar=true.
	DirectUnpack bool
	// DirectUnpackThreads limits the number of concurrent DirectUnpack
	// workers across all jobs. 0 means no limit (one per active job).
	// Default 3 (matching SABnzbd).
	DirectUnpackThreads int

	// ScriptStage metadata injected into SAB_* env vars.
	Version    string
	APIKey     string
	ListenAddr string
}

// FileComplete is emitted on Application.FileComplete() when a file is done.
type FileComplete struct {
	JobID   string
	FileIdx int
	// CRC32 is the whole-file CRC32 computed by the assembler from
	// per-article CRCs combined in offset order. Zero if unavailable
	// (e.g. UU-encoded articles or failed articles).
	CRC32 uint32
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
	// NzoID is set on per-job events (currently job_finalized) so clients
	// can target a specific row without a full refetch.
	NzoID string `json:"nzo_id,omitempty"`
	Tool  string `json:"tool,omitempty"`  // subprocess tool name (par2, unrar, 7z, script)
	Line  string `json:"line,omitempty"`  // single output line from subprocess
	Stage string `json:"stage,omitempty"` // pipeline stage name (repair, unpack)
	// Servers is populated on "metrics" events with per-server
	// connection snapshots so the UI can render the server status
	// panel without a separate HTTP poll.
	Servers []downloader.ServerSnapshot `json:"servers,omitempty"`
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

	// unrarHasProblem caches the result of DetectUnrar at startup.
	// True when the binary is non-original or too old (< 5.50).
	unrarHasProblem bool
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
		directUnpackers:      make(map[string]*directunpack.DirectUnpacker),
		unrarHasProblem:      unpack.DetectUnrar(context.Background(), cfg.UnrarCommand).HasProblem,
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

	// Probe sparse file support on the download directory. Pre-allocation
	// uses fallocate/ftruncate which benefits from sparse-capable filesystems.
	if supported, msg := assembler.CheckSparseSupport(cfg.DownloadDir); !supported {
		log.Warn(msg)
	} else {
		log.Info(msg)
	}

	if cfg.WriteCacheBytes > 0 {
		log.Info("write coalescing enabled",
			"cacheMiB", cfg.WriteCacheBytes/(1024*1024))
	}

	servers := make([]*downloader.Server, len(cfg.Servers))
	for i, sc := range cfg.Servers {
		servers[i] = downloader.NewServer(sc)
	}
	d := downloader.New(q, servers, app.meter, app.buildDownloaderOptions(), log)
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
		built, err := buildStages(cfg, log)
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
			app.emitter.Broadcast(Event{
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
		MinFreeBytes:       cfg.MinFreeSpace,
		WriteCacheBytes:    cfg.WriteCacheBytes,
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

// downloadCompleteness returns the percentage (0-100) of a job's bytes that
// were successfully retrieved. failedBytes is the number of bytes that could
// not be fetched from any configured server. Returns 0 when totalBytes is
// non-positive (nothing downloaded) or when every byte failed.
func downloadCompleteness(totalBytes, failedBytes int64) int64 {
	if totalBytes <= 0 || failedBytes >= totalBytes {
		return 0
	}
	if failedBytes < 0 {
		failedBytes = 0
	}
	return int64(float64(totalBytes-failedBytes) / float64(totalBytes) * 100)
}

// finalizeJob is called by the post-processor when a job is done (success
// or failure). It builds a history entry, persists the job payload for
// retry support, writes to the history DB, removes the job from the
// active queue, fires WebSocket events, and dispatches notifications.
//
// This was extracted from the OnJobDone closure in New() to make the
// history-entry construction and notification logic independently testable.
func (app *Application) finalizeJob(job *postproc.Job) {
	log := app.log

	stageLogJSON, _ := json.Marshal(job.StageLog)
	var downloadDuration int64
	if !job.Queue.DownloadStarted.IsZero() && !job.Queue.DownloadFinished.IsZero() {
		downloadDuration = int64(job.Queue.DownloadFinished.Sub(job.Queue.DownloadStarted).Seconds())
	}
	if downloadDuration == 0 {
		downloadDuration = 1
	}

	// Compute post-processing duration from stage log.
	var postprocDuration int64
	for _, se := range job.StageLog {
		postprocDuration += int64(se.Elapsed.Seconds())
	}

	// Download health: the fraction of bytes successfully retrieved.
	// FailedBytes counts bytes that could not be fetched from any server
	// (exhausted on all). This is deliberately byte-based rather than
	// article-based: a failed article is marked both Done and Failed (Done
	// means "resolved", not "succeeded"), so counting done articles would
	// always report ~100% on a finished job regardless of missing data.
	completeness := downloadCompleteness(job.Queue.TotalBytes, job.Queue.FailedBytes)
	downloaded := job.Queue.TotalBytes - job.Queue.FailedBytes - job.Queue.RemainingBytes

	serverStatsParts := make([]string, 0, len(job.Queue.ServerStats))
	// Sort keys for deterministic output in history entries.
	serverNames := make([]string, 0, len(job.Queue.ServerStats))
	for s := range job.Queue.ServerStats {
		serverNames = append(serverNames, s)
	}
	slices.Sort(serverNames)
	for _, s := range serverNames {
		b := job.Queue.ServerStats[s]
		serverStatsParts = append(serverStatsParts, fmt.Sprintf("%s=%.1f MB", s, float64(b)/(1024*1024)))
	}
	serverStats := strings.Join(serverStatsParts, ", ")
	repairSummary := ""
	for _, stageEntry := range job.StageLog {
		if stageEntry.Stage == "repair" {
			if stageEntry.Err != nil {
				repairSummary = fmt.Sprintf("Repair failed: %v", stageEntry.Err)
			} else {
				repairSummary = "Repair OK"
				if len(stageEntry.Lines) > 0 {
					repairSummary = stageEntry.Lines[0]
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
		Storage:      job.FinalDir,
		Path:         job.FinalDir,
		DownloadTime: downloadDuration,
		PostprocTime: postprocDuration,
		StageLog:     string(stageLogJSON),
		Bytes:        job.Queue.TotalBytes,
		Downloaded:   downloaded,
		Completeness: completeness,
		TimeAdded:    job.Queue.Added,
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
	if err := app.queue.Remove(job.Queue.ID); err != nil {
		log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
	}
	select {
	case app.postProcComplete <- PostProcComplete{JobID: job.Queue.ID}:
	default:
	}
	// job_finalized signals a queue→history transition: both
	// stores subscribe to it and refresh from a single trigger,
	// so they reach the new state together.
	app.emitter.Broadcast(Event{Type: "job_finalized", NzoID: job.Queue.ID})

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
}

// Queue returns the application's download queue.
func (app *Application) Queue() *queue.Queue { return app.queue }

// Speed returns the current aggregate download speed in bytes/sec, or 0
// when downloading is idle or the downloader has not been wired yet.
func (app *Application) Speed() float64 {
	if app.downloader == nil {
		return 0
	}
	return app.downloader.Speed()
}

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
	downloadDir := app.cfg.DownloadDir
	completeDir := app.cfg.CompleteDir
	categories := app.cfg.Categories
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
		path := filepath.Join(app.cfg.DownloadDir, snap.Name)
		_ = os.RemoveAll(path)
	}
	app.emitter.Broadcast(Event{Type: "queue_updated"})

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

	succeeded = true
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
			// Trigger a table refresh only while actively downloading so
			// individual job percentages update, but avoid pointless
			// refreshes when idle or paused.
			remaining := app.queue.TotalRemainingBytes()
			speed := app.downloader.Speed()
			app.emitter.Broadcast(Event{
				Type:          "metrics",
				Speed:         int64(speed),
				Remaining:     remaining,
				SpeedLimit:    app.downloader.SpeedLimit(),
				BandwidthMax:  app.bandwidthMax.Load(),
				BandwidthPerc: int(app.bandwidthPerc.Load()),
				Servers:       app.downloader.ServerStatus(),
			})
			if speed > 0 {
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
	if err := app.queue.Save(filepath.Join(app.cfg.AdminDir, "queue")); err != nil {
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
	app.emitter.Broadcast(Event{Type: "queue_updated"})

	// DirectUnpack: feed completed RAR volumes to the unpacker for
	// streaming extraction during download.
	if app.cfg.DirectUnpack && app.cfg.EnableUnrar {
		app.maybeDirectUnpack(fc)
	}

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
		limit := app.cfg.DirectUnpackThreads
		if limit > 0 && int(app.activeDU.Load()) >= limit {
			app.mu.Unlock()
			app.log.Debug("directunpack: skipping, concurrency limit reached",
				"job", fc.JobID, "active", app.activeDU.Load(), "limit", limit)
			return
		}
		downloadDir := filepath.Join(app.cfg.DownloadDir, snap.Name)
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
	return directunpack.Options{
		Password:         "", // per-job passwords are pre-checked; DU skips password jobs
		OneFolder:        app.cfg.FlatUnpack,
		OverwriteFiles:   app.cfg.OverwriteFiles,
		IgnoreUnrarDates: app.cfg.IgnoreUnrarDates,
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
		if saveErr := app.queue.Save(filepath.Join(app.cfg.AdminDir, "queue")); saveErr != nil {
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

	downloadDir := filepath.Join(app.cfg.DownloadDir, job.Name)

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

	cat := config.FindCategory(app.cfg.Categories, job.Category)
	catDir := cat.Dir
	// P6: Category dir trailing '*' suppresses the per-job subfolder.
	// Files go directly into the category directory ("flat layout").
	// e.g. catDir="movies*" → complete_dir/movies/file.mkv
	//      catDir="movies"  → complete_dir/movies/JobName/file.mkv
	flatLayout := strings.HasSuffix(catDir, "*")
	if flatLayout {
		catDir = strings.TrimSuffix(catDir, "*")
	}
	finalDir := filepath.Join(app.cfg.CompleteDir, catDir, job.Name)
	if flatLayout {
		finalDir = filepath.Join(app.cfg.CompleteDir, catDir)
	}
	// Collect DirectUnpack results (if any) before enqueuing post-processing.
	// Wait() blocks until any in-progress extraction finishes.
	var duResults map[string]directunpack.SuccessSet
	var duFailures map[string]directunpack.FailedSet
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
		if len(duResults) > 0 {
			app.log.Info("directunpack: passing results to postproc",
				"job", job.ID, "sets", len(duResults))
		}
		if len(duFailures) > 0 {
			app.log.Warn("directunpack: passing failures to postproc",
				"job", job.ID, "failed_sets", len(duFailures))
		}
	}

	app.postProcessor.Process(&postproc.Job{
		Queue:                job,
		DownloadDir:          downloadDir,
		FinalDir:             finalDir,
		Sanitize:             app.cfg.Sanitize,
		FailMsg:              failMsg,
		DirectUnpackSets:     duResults,
		DirectUnpackFailures: duFailures,
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
func (app *Application) SetQuickCheckEnabled(enabled bool) {
	if app.quickCheckStage != nil {
		app.quickCheckStage.SetEnabled(enabled)
	}
}

// SetParCleanup enables or disables par2 file deletion for future jobs.
// Thread-safe; takes effect immediately without restart.
func (app *Application) SetParCleanup(enabled bool) {
	if app.par2CleanupStage != nil {
		app.par2CleanupStage.SetCleanup(enabled)
	}
}

// SetRarCleanup enables or disables archive file deletion for future jobs.
// Thread-safe; takes effect immediately without restart.
// No-op when no unpack stage is configured (unrar/7z disabled at startup).
func (app *Application) SetRarCleanup(enabled bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetCleanup(enabled)
	}
}

// SetOverwriteFiles enables or disables overwriting existing files on extraction.
func (app *Application) SetOverwriteFiles(enabled bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetOverwriteFiles(enabled)
	}
}

// SetFlatUnpack enables or disables flat (directory-ignoring) extraction.
func (app *Application) SetFlatUnpack(enabled bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetFlatUnpack(enabled)
	}
}

// SetPermissions updates the octal permission string applied after extraction.
func (app *Application) SetPermissions(v string) {
	if app.unpackStage != nil {
		app.unpackStage.SetPermissions(v)
	}
}

// SetFolderRename enables or disables the _UNPACK_/_FAILED_ prefix behavior.
func (app *Application) SetFolderRename(enabled bool) {
	if app.finalizeStage != nil {
		app.finalizeStage.SetFolderRename(enabled)
	}
}

// SetScriptCanFail controls whether non-zero script exit codes fail the job.
func (app *Application) SetScriptCanFail(enabled bool) {
	if app.scriptStage != nil {
		app.scriptStage.SetScriptCanFail(enabled)
	}
}

// SetUseGoRAR enables or disables pure-Go RAR extraction at runtime.
func (app *Application) SetUseGoRAR(v bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetUseGoRAR(v)
	}
}

// SetUseGo7z enables or disables pure-Go 7-Zip extraction at runtime.
func (app *Application) SetUseGo7z(v bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetUseGo7z(v)
	}
}

// SetUseGoPar2 enables or disables pure-Go par2 verification/repair at runtime.
func (app *Application) SetUseGoPar2(v bool) {
	if app.repairStage != nil {
		app.repairStage.SetUseGoPar2(v)
	}
}

// SetGoRarFallback enables or disables fallback to unrar binary.
func (app *Application) SetGoRarFallback(v bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetGoRarFallback(v)
	}
}

// SetGo7zFallback enables or disables fallback to 7z binary.
func (app *Application) SetGo7zFallback(v bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetGo7zFallback(v)
	}
}

// SetGoPar2Fallback enables or disables fallback to par2 binary.
func (app *Application) SetGoPar2Fallback(v bool) {
	if app.repairStage != nil {
		app.repairStage.SetGoPar2Fallback(v)
	}
}

// SetNiceAndIonice updates nice and ionice command prefixes for external tools.
func (app *Application) SetNiceAndIonice(nice, ionice string) {
	if app.repairStage != nil {
		app.repairStage.SetNiceAndIonice(nice, ionice)
	}
	if app.unpackStage != nil {
		app.unpackStage.SetNiceAndIonice(nice, ionice)
	}
}

// SetExternalCommands updates the paths to par2, unrar, and 7z binaries.
func (app *Application) SetExternalCommands(par2Cmd, unrarCmd, sevenzCmd string) {
	if app.repairStage != nil {
		app.repairStage.SetPar2Command(par2Cmd)
	}
	if app.unpackStage != nil {
		app.unpackStage.SetUnrarCommand(unrarCmd)
		app.unpackStage.SetSevenZipCommand(sevenzCmd)
	}
}

// SetExtraParams parses and applies extra command parameters to unrar and par2.
func (app *Application) SetExtraParams(unrarParams, par2Params string) {
	if app.repairStage != nil {
		extraPar2Args, err := cmdutil.ParseExtraParams(par2Params)
		if err == nil {
			app.repairStage.SetExtraPar2Params(extraPar2Args)
		} else {
			app.log.Warn("Failed to parse extra par2 params", "err", err)
		}
	}
	if app.unpackStage != nil {
		extraUnrarArgs, err := cmdutil.ParseExtraParams(unrarParams)
		if err == nil {
			if err := cmdutil.ValidateUnrarParams(extraUnrarArgs); err != nil {
				app.log.Warn("extra_unrar_params contains non-standard flags", "err", err)
			}
			app.unpackStage.SetExtraUnrarParams(extraUnrarArgs)
		} else {
			app.log.Warn("Failed to parse extra unrar params", "err", err)
		}
	}
}

// SetCleanupExtensions updates the file extension cleanup list.
func (app *Application) SetCleanupExtensions(exts []string) {
	if app.cleanupStage != nil {
		app.cleanupStage.SetExtensions(exts)
	}
}

// SetDeobfuscate enables or disables filename deobfuscation.
func (app *Application) SetDeobfuscate(enabled bool) {
	if app.deobfuscateStage != nil {
		app.deobfuscateStage.SetEnabled(enabled)
	}
}

// SetIgnoreSamples enables or disables automatic sample cleanup.
func (app *Application) SetIgnoreSamples(enabled bool) {
	if app.sampleStage != nil {
		app.sampleStage.SetEnabled(enabled)
	}
}

// SetScriptDir updates the user scripts directory.
func (app *Application) SetScriptDir(dir string) {
	if app.scriptStage != nil {
		app.scriptStage.SetScriptDir(dir)
	}
}

// SetUnpackEnabled enables or disables the unpack stage at runtime.
func (app *Application) SetUnpackEnabled(enabled bool) {
	if app.unpackStage != nil {
		app.unpackStage.SetEnabled(enabled)
	}
}

// SetPasswordFile updates the unpack password file at runtime.
func (app *Application) SetPasswordFile(v string) {
	if app.unpackStage != nil {
		app.unpackStage.SetPasswordFile(v)
	}
}

// SetEnableFileJoin enables or disables split file joining at runtime.
func (app *Application) SetEnableFileJoin(v bool) {
	app.mu.Lock()
	app.cfg.EnableFileJoin = v
	app.mu.Unlock()
	if app.unpackStage != nil {
		app.unpackStage.SetEnableFileJoin(v)
	}
}

// SetEnableRecursive enables or disables recursive unpacking at runtime.
func (app *Application) SetEnableRecursive(v bool) {
	app.mu.Lock()
	app.cfg.EnableRecursive = v
	app.mu.Unlock()
	if app.unpackStage != nil {
		app.unpackStage.SetEnableRecursive(v)
	}
}

// SetDirectUnpack enables or disables extraction of RAR archives while still downloading.
func (app *Application) SetDirectUnpack(v bool) {
	app.mu.Lock()
	app.cfg.DirectUnpack = v
	app.mu.Unlock()
}

// SetDirectUnpackThreads limits the number of concurrent DirectUnpack workers.
func (app *Application) SetDirectUnpackThreads(v int) {
	app.mu.Lock()
	app.cfg.DirectUnpackThreads = v
	app.mu.Unlock()
}

// SetEnableUnrar enables or disables standard RAR unpacking at runtime.
func (app *Application) SetEnableUnrar(v bool) {
	app.mu.Lock()
	app.cfg.EnableUnrar = v
	app.mu.Unlock()
}

// SetEnable7zip enables or disables standard 7-Zip unpacking at runtime.
func (app *Application) SetEnable7zip(v bool) {
	app.mu.Lock()
	app.cfg.Enable7zip = v
	app.mu.Unlock()
}

// SetPar2Turbo updates par2cmdline-turbo settings at runtime. Thread-safe.
func (app *Application) SetPar2Turbo(v bool) {
	app.mu.Lock()
	app.cfg.Par2Turbo = v
	app.mu.Unlock()
	if app.repairStage != nil {
		app.repairStage.SetPar2Turbo(v)
	}
}

// SetIgnoreUnrarDates updates unrar modify timestamps options at runtime. Thread-safe.
func (app *Application) SetIgnoreUnrarDates(v bool) {
	app.mu.Lock()
	app.cfg.IgnoreUnrarDates = v
	app.mu.Unlock()
	if app.unpackStage != nil {
		app.unpackStage.SetIgnoreUnrarDates(v)
	}
}

// SetSanitizeOptions updates the filename sanitization options used for new
// jobs. Thread-safe; takes effect for the next enqueued job.
func (app *Application) SetSanitizeOptions(opts fsutil.SanitizeOptions) {
	app.mu.Lock()
	app.cfg.Sanitize = opts
	app.mu.Unlock()
}

// SetMinFreeSpace updates the low-disk-space threshold. Thread-safe.
func (app *Application) SetMinFreeSpace(bytes int64) {
	app.mu.Lock()
	app.cfg.MinFreeSpace = bytes
	app.mu.Unlock()
	if app.assembler != nil {
		app.assembler.SetMinFreeBytes(bytes)
	}
}

// SetMaxArtTries updates per-article retry limit and related dispatch options.
// Thread-safe; takes effect on the next dispatch pass.
func (app *Application) SetMaxArtTries(v int) {
	app.mu.Lock()
	app.cfg.MaxArtTries = v
	app.mu.Unlock()
	app.pushDispatchOptions()
}

// SetMaxArtOpt updates the backup-server retry limit.
func (app *Application) SetMaxArtOpt(v int) {
	app.mu.Lock()
	app.cfg.MaxArtOpt = v
	app.mu.Unlock()
	app.pushDispatchOptions()
}

// SetTopOnly controls whether dispatch is restricted to the top-priority server.
func (app *Application) SetTopOnly(v bool) {
	app.mu.Lock()
	app.cfg.TopOnly = v
	app.mu.Unlock()
	app.pushDispatchOptions()
}

// SetPropagationDelay updates the delay before new jobs start downloading.
func (app *Application) SetPropagationDelay(minutes int) {
	app.mu.Lock()
	app.cfg.PropagationDelay = minutes
	app.mu.Unlock()
	app.pushDispatchOptions()
}

// ReloadPostProcOptions applies all hot-applicable postproc settings from cfg
// to the running pipeline. Called by the API on any postproc config save; it
// re-applies every setting, not just the one that changed — all setters are
// idempotent so this is safe. Do not add setters with significant side effects
// (logging, I/O) without reviewing this call site.
func (app *Application) ReloadPostProcOptions(cfg *config.Config) {
	app.SetQuickCheckEnabled(cfg.PostProc.EnableQuickCheck)
	app.SetParCleanup(cfg.PostProc.EnableParCleanup)
	app.SetRarCleanup(cfg.PostProc.EnableRarCleanup)
	app.SetOverwriteFiles(cfg.PostProc.OverwriteFiles)
	app.SetFlatUnpack(cfg.PostProc.FlatUnpack)
	app.SetPermissions(cfg.PostProc.Permissions)
	app.SetFolderRename(cfg.PostProc.FolderRename)
	app.SetScriptCanFail(cfg.PostProc.ScriptCanFail)

	// --- NEW HOT-RELOADS ---
	app.SetUseGoRAR(cfg.PostProc.UseGoRAR)
	app.SetUseGo7z(cfg.PostProc.UseGo7z)
	app.SetUseGoPar2(cfg.PostProc.UseGoPar2)
	app.SetGoRarFallback(cfg.PostProc.GoRarFallback)
	app.SetGo7zFallback(cfg.PostProc.Go7zFallback)
	app.SetGoPar2Fallback(cfg.PostProc.GoPar2Fallback)
	app.SetNiceAndIonice(cfg.PostProc.Nice, cfg.PostProc.Ionice)
	app.SetExternalCommands(cfg.PostProc.Par2Command, cfg.PostProc.UnrarCommand, cfg.PostProc.SevenzCommand)
	app.SetExtraParams(cfg.PostProc.ExtraUnrarParams, cfg.PostProc.ExtraPar2Params)
	app.SetCleanupExtensions(cfg.PostProc.CleanupExtensions)
	app.SetDeobfuscate(cfg.PostProc.DeobfuscateFilenames)
	app.SetIgnoreSamples(cfg.PostProc.IgnoreSamples)
	app.SetScriptDir(cfg.General.ScriptDir)
	app.SetUnpackEnabled(cfg.PostProc.EnableUnrar || cfg.PostProc.Enable7zip || cfg.PostProc.EnableFileJoin)
	app.SetPasswordFile(cfg.PostProc.PasswordFile)
	app.SetEnableFileJoin(cfg.PostProc.EnableFileJoin)
	app.SetEnableRecursive(cfg.PostProc.EnableRecursive)
	app.SetDirectUnpack(cfg.PostProc.DirectUnpack)
	app.SetDirectUnpackThreads(cfg.PostProc.DirectUnpackThreads)
	app.SetEnableUnrar(cfg.PostProc.EnableUnrar)
	app.SetEnable7zip(cfg.PostProc.Enable7zip)
	app.SetPar2Turbo(cfg.PostProc.Par2Turbo)
	app.SetIgnoreUnrarDates(cfg.PostProc.IgnoreUnrarDates)
}

// ReloadDownloadOptions applies all hot-applicable download settings from cfg
// to the running pipeline. Same idempotency note as ReloadPostProcOptions.
func (app *Application) ReloadDownloadOptions(cfg *config.Config) {
	app.SetSanitizeOptions(cfg.Downloads.SanitizeOptions())
	app.SetMinFreeSpace(int64(cfg.Downloads.MinFreeSpace))
	app.SetMaxArtTries(cfg.Downloads.MaxArtTries)
	app.SetMaxArtOpt(cfg.Downloads.MaxArtOpt)
	app.SetTopOnly(cfg.Downloads.TopOnly)
	app.SetPropagationDelay(cfg.Downloads.PropagationDelay)
}

// ReloadGeneralOptions applies all hot-applicable general settings from cfg
// to the running logging handlers.
func (app *Application) ReloadGeneralOptions(cfg *config.Config) {
	globalLevel, err := cfg.General.ParseLogLevel()
	if err != nil {
		app.log.Error("failed to parse global log level on reload", "err", err)
		return
	}
	compLevels, err := cfg.General.ParseLogLevels()
	if err != nil {
		app.log.Error("failed to parse component log levels on reload", "err", err)
		return
	}
	SetLogLevels(globalLevel, compLevels)
}

// pushDispatchOptions reads the current mutable dispatch fields under app.mu
// and forwards them to the running downloader. Must not be called while
// holding app.mu.
func (app *Application) pushDispatchOptions() {
	app.mu.Lock()
	maxArtTries := app.cfg.MaxArtTries
	maxArtOpt := app.cfg.MaxArtOpt
	topOnly := app.cfg.TopOnly
	propDelay := time.Duration(app.cfg.PropagationDelay) * time.Minute
	app.mu.Unlock()
	if app.downloader != nil {
		app.downloader.SetDispatchOptions(maxArtTries, maxArtOpt, topOnly, propDelay)
	}
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
	app.emitter.Broadcast(Event{Type: "queue_updated"})
	app.emitter.Broadcast(Event{Type: "history_updated"})
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
	newDownloader := downloader.New(app.queue, servers, app.meter, app.buildDownloaderOptions(), app.log)
	if err := newDownloader.Start(app.ctx); err != nil {
		return err
	}
	app.downloader = newDownloader
	app.pipeline.setCompletions(newDownloader.Completions())
	return nil
}

// buildDownloaderOptions constructs a downloader.Options from the current
// app config. Used by both New() and ReloadDownloader() to ensure the same
// options are applied consistently.
func (app *Application) buildDownloaderOptions() downloader.Options {
	q := app.queue
	return downloader.Options{
		MaxArtTries:      app.cfg.MaxArtTries,
		MaxArtOpt:        app.cfg.MaxArtOpt,
		TopOnly:          app.cfg.TopOnly,
		NoPenalties:      app.cfg.NoPenalties,
		PreCheck:         app.cfg.PreCheck,
		PropagationDelay: time.Duration(app.cfg.PropagationDelay) * time.Minute,
		OnJobHopeless: func(jobID string) {
			snap := q.SnapshotJob(jobID)
			if snap == nil {
				return
			}
			app.maybeFinalize(jobID, failMsgForJob(snap))
		},
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
