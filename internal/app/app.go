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
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/types"
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
	// Stop/setCompletions/ClearAllEmitted/Start sequences, which would
	// otherwise risk wiring app.downloader and app.pipeline's completions
	// source to two different downloader instances.
	reloadMu sync.Mutex
	config   *config.Config
	emitter  EventEmitter
	meter    *bpsmeter.Meter

	queue           *queue.Queue
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
	onFileComplete       func(jobID string, fileIdx int, fileCRC uint32)
	markArticlesDoneHook func(jobID string, messageIDs []string) error

	wg     sync.WaitGroup
	ctx    context.Context //nolint:containedctx // ctx is the app's lifecycle context, stored by design
	cancel context.CancelFunc

	started atomic.Bool
	stopped atomic.Bool

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
	var qOpts []queue.Option
	if repo != nil && repo.DB() != nil {
		store := queue.NewSQLiteStore(repo.DB(), queueStateDir, repo)
		qOpts = append(qOpts, queue.WithStore(store))
	}
	if maxActiveJobs > 0 {
		qOpts = append(qOpts, queue.WithMaxActiveJobs(maxActiveJobs))
	}
	qOpts = append(qOpts, queue.WithLogger(log), queue.WithSanitizeOptions(sanitize))
	q, err := queue.Load(queueStateDir, qOpts...)
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
		queue:       q,
		completions: app.downloader.Completions(),
		downloadDir: dlDir,
		sanitize:    sanitize,
		onHeartbeat: app.RecordHeartbeat,
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
		OnJobDone: app.finalizer.finalize,
		Logger:    log,
	})
	app.postProcessor = pp

	onFileComplete := func(jobID string, fileIdx int, fileCRC uint32) {
		fc := FileComplete{JobID: jobID, FileIdx: fileIdx, CRC32: fileCRC}
		select {
		case app.internalFileComplete <- fc:
		default:
			// Channel full — spawn goroutine on app.wg to ensure delivery.
			// Ordering constraint: this is safe w.r.t. wg.Add-during-Wait only because OnFileComplete
			// runs on the assembler worker, which Shutdown joins at step 2 — before app.wg.Wait() at step 4.
			app.wg.Go(func() {
				app.internalFileComplete <- fc
			})
		}
	}
	app.onFileComplete = onFileComplete

	markDone := q.MarkArticlesDone
	markDoneByIdx := q.MarkArticlesDoneByIdx
	if app.markArticlesDoneHook != nil {
		markDone = app.markArticlesDoneHook
		markDoneByIdx = nil
	}

	asm := assembler.New(assembler.Options{
		FileInfo:                p.resolveFileInfo,
		MarkArticlesDoneByIdx:   markDoneByIdx,
		MarkArticlesFailedByIdx: q.MarkArticlesFailedByIdx,
		MarkArticlesDone:        markDone,
		MarkArticlesFailed:      q.MarkArticlesFailed,
		SetWriteCursor:          q.SetFileWriteCursor,
		MinFreeBytes:            minFreeBytes,
		WriteCacheBytes:         writeCacheBytes,
		OnLowDisk:               app.handleLowDisk,
		OnFileComplete:          onFileComplete,
	}, log)
	app.assembler = asm
	p.assembler = asm
	app.diskProbe = assembler.NewDiskProbe(assembler.DefaultDiskProbeTTL)
	app.RecordHeartbeat()

	return app, nil
}

// WithCheckpointInterval returns an option that sets the queue persistence interval.
func WithCheckpointInterval(d time.Duration) func(*Application) {
	return func(a *Application) { a.checkpointInterval = d }
}

// WithMarkArticlesDoneHook returns an option that overrides the assembler's
// batched MarkArticlesDone callback. The provided hook is responsible for
// persisting the article completion status to the queue.
func WithMarkArticlesDoneHook(hook func(jobID string, messageIDs []string) error) func(*Application) {
	return func(a *Application) { a.markArticlesDoneHook = hook }
}

// Queue returns the application's download queue.
func (app *Application) Queue() *queue.Queue { return app.queue }

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
func (app *Application) detectDuplicateNZB(ctx context.Context, job *queue.Job, force bool, nzbDir string) (isDuplicate bool, warning string) {
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
	if !isDuplicate {
		return false, ""
	}
	app.log.Info("duplicate NZB detected", "filename", job.Filename, "md5", job.MD5, "reason", dupReason, "forced", force)
	if !force {
		return true, "Duplicate NZB"
	}
	return true, "Duplicate NZB (Forced)"
}

// AddJob validates, deduplicates, and enqueues a new download job. If force
// is false and a duplicate is detected, the job is added in a paused state.
func (app *Application) AddJob(ctx context.Context, job *queue.Job, rawNZB []byte, force bool) error {
	adminDir := app.config.GetGeneral().AdminDir
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		return fmt.Errorf("app: mkdir admin nzb: %w", err)
	}

	isDuplicate, warning := app.detectDuplicateNZB(ctx, job, force, nzbDir)
	if isDuplicate {
		if !force {
			job.Status = constants.StatusPaused
		}
		job.Warning = warning
	}
	// Pick a name not already taken in the queue or on disk. queue.Add
	// re-checks under its write lock (authoritative), so the small TOCTOU
	// window here is limited to filesystem collisions which are benign.
	snap := app.config.Snapshot()
	gen := &snap.General
	downloadDir := gen.DownloadDir
	completeDir := gen.CompleteDir
	categories := snap.Categories
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
	// Write the backup for every job that enters the queue, duplicate or
	// not. It used to be skipped for duplicates, which meant a forced
	// re-add downloaded normally and finalized with no NZB on disk — and
	// since the backup is the only surviving copy of the article
	// message-IDs once the manifest is unlinked, that job was silently
	// unretryable. A non-forced duplicate is enqueued paused and can still
	// be resumed, so it needs one too.
	if job.Filename != "" && len(rawNZB) > 0 {
		name, err := writeNZBBackup(nzbDir, job.Filename, rawNZB)
		if err != nil {
			// Not fatal: the download itself does not need the backup.
			// job.NZBBackup stays empty and RetryHistoryJob reports the
			// absence rather than failing obscurely later.
			app.log.Warn("failed to write gzipped NZB backup; job will not be retryable",
				"filename", job.Filename, "err", err)
		} else {
			job.NZBBackup = name
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

// RemoveJob cancels and removes a job from the queue, optionally deleting its download directory.
func (app *Application) RemoveJob(ctx context.Context, id string, deleteFiles bool) error {
	snap := app.queue.SnapshotJob(id)
	if snap == nil {
		return fmt.Errorf("job %q not found", id)
	}
	// Abort any active DirectUnpacker for this job before removing files.
	app.duOrch.abortJob(id)

	// Cancel in-flight post-processing and assembler file handles before
	// removing files to prevent the PP from operating on a deleted directory.
	app.postProcessor.Cancel(id)
	// CancelJob blocks until the assembler has actually closed the job's
	// open file handles. If it returns an error (ctx cancelled, assembler
	// not started/stopped), we have no such guarantee -- warn so a
	// subsequent directory-delete failure (e.g. .nfsXXXXXX on NFS mounts)
	// is traceable back to this race instead of looking unexplained.
	if err := app.assembler.CancelJob(ctx, id); err != nil {
		app.log.Warn("assembler cancel job did not confirm file handles closed",
			"job", id, "error", err)
	}
	// Remove from queue and pipeline so no more articles are dispatched.
	if err := app.queue.Remove(id); err != nil {
		return err
	}
	app.pipeline.forgetJob(id)
	if deleteFiles {
		downloadDir := app.config.GetGeneral().DownloadDir
		path := filepath.Join(downloadDir, snap.Name)
		if err := safeDeleteDir(path, downloadDir); err != nil {
			app.log.Warn("failed to delete job directory", "path", path, "err", err)
		}
	}
	app.emit(Event{Type: "queue_updated"})

	// Disconnect NNTP servers if no downloadable jobs remain.
	if !app.queue.HasDownloadableJobs() {
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
		if app.cancel != nil {
			app.cancel()
		}
		app.started.Store(false)
	}()

	//nolint:gosec // G118: cancel is stored on the struct rather than a local,
	// so gosec cannot see the calls. It is invoked by Shutdown on the success
	// path and by the failure defer above on every error return.
	app.ctx, app.cancel = context.WithCancel(ctx)
	if err := app.assembler.Start(app.ctx); err != nil {
		return err
	}
	// Reset transient download state: Downloading → Queued, Emitted → false.
	// On a cold restart these flags are stale — the old downloader's
	// in-flight articles are long gone.
	app.queue.ClearAllEmitted()
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

	// Sweep expired history, after the reconciliation above and not before.
	//
	// That loop identifies a crash between the history commit and
	// Queue.Remove by looking a still-queued completed job up in history,
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
//
// stopWorkers stops the downloader, aborts active DirectUnpackers, and stops
// the assembler in exact documented order. Shared between Shutdown and
// ForceStopWorkers (in export_test.go) to guarantee consistent teardown ordering.
func (app *Application) stopWorkers(stepTimeout time.Duration, errs *[]error) {
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

	if dl != nil {
		if err := waitBounded("downloader", stepTimeout, dl.Stop, app.log); err != nil && errs != nil {
			*errs = append(*errs, fmt.Errorf("downloader stop: %w", err))
		}
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
	const stepTimeout = 15 * time.Second

	app.stopWorkers(stepTimeout, &errs)

	app.cancel()

	if err := waitBounded("wg.Wait", stepTimeout, func() error {
		app.wg.Wait()
		return nil
	}, app.log); err != nil {
		errs = append(errs, fmt.Errorf("wg wait: %w", err))
	}

	if err := waitBounded("postprocessor", stepTimeout, app.postProcessor.Stop, app.log); err != nil {
		errs = append(errs, fmt.Errorf("postprocessor stop: %w", err))
	}

	adminDir := app.config.GetGeneral().AdminDir
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
			adminDir := app.config.GetGeneral().AdminDir
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
// logQueueWriteFailure reports a failed per-file queue write at a level that
// matches what the failure means.
//
// A job that was removed, or whose manifest was evicted, between the
// assembler producing this event and the queue receiving it is ordinary — the
// queue methods report it as ErrNotFound/ErrJobNotResident precisely so a
// caller can recognise it rather than having it arrive as a silent nil (#261).
// Those get Debug. Anything else is a write that should have landed and did
// not, and gets Warn.
func (app *Application) logQueueWriteFailure(op, jobID string, fileIdx int, err error) {
	if errors.Is(err, queue.ErrNotFound) || errors.Is(err, queue.ErrJobNotResident) {
		app.log.Debug(op+" skipped, job no longer resident in the queue",
			"job", jobID, "fileidx", fileIdx, "err", err)
		return
	}
	app.log.Warn(op+" failed", "job", jobID, "fileidx", fileIdx, "err", err)
}

func (app *Application) handleFileComplete(ctx context.Context, fc FileComplete) {
	// Store the assembled CRC32 on the queue's JobFile so it survives
	// serialization and is available during post-processing QuickCheck.
	if fc.CRC32 != 0 {
		if err := app.queue.SetFileCRC32(fc.JobID, fc.FileIdx, fc.CRC32); err != nil {
			// A job removed or evicted between assembly and here is ordinary
			// and not worth a warning; anything else means the CRC is lost
			// and QuickCheck will have less to verify against.
			app.logQueueWriteFailure("set file CRC32", fc.JobID, fc.FileIdx, err)
		}
	}
	if err := app.queue.MarkFileComplete(fc.JobID, fc.FileIdx); err != nil {
		// Returning bare here was itself the shape #261 describes: the file
		// never gets marked complete, no event is emitted, and nothing says
		// why.
		app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
		return
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
	if !snap.HasDeferredPar2() || snap.Progress().Par2Recovered() {
		return false
	}
	cfgSnap := app.config.Snapshot()
	downloadDir := cfgSnap.General.DownloadDir
	pp := &cfgSnap.PostProc
	parseOpts := par2.ParseOptionsFromConfig(pp)
	dir := filepath.Join(downloadDir, snap.Name)

	// An unreadable manifest means the CRC comparison cannot be made here, so
	// this job finalizes with its recovery volumes still deferred. That is
	// the outcome, not a fallback that failed: an unreadable manifest is only
	// observable for a non-resident job — a resident one holds its manifest
	// in memory and never re-reads the file — and UndeferRecoveryVolumes is
	// manifest-tier, so the very non-residency that exposes the unreadable
	// manifest also makes the un-defer below fail. Hydrating first does not
	// help; the manifest is what could not be read.
	//
	// This is survivable because verification is not skipped, only moved.
	// Post-processing's quickcheck stage hits the same unreadable manifest
	// and records QuickCheckInconclusive rather than staying silent, which
	// forces the repair stage to run a full par2 verify against the index
	// files already on disk (#294). The missing recovery volumes mean par2
	// can detect damage but not repair it, so a corrupt job is reported
	// rather than shipped.
	//
	// The branch used to argue the opposite — that finalizing meant shipping
	// "a repair nothing checked for" — and set out to fetch the volumes
	// anyway. It could never do that, and the premise was wrong besides.
	// See TestMaybeReleaseRecoveryVolumes_UnreadableManifest.
	needsRecovery, reason := true, "manifest unreadable, cannot verify integrity"
	if m, mErr := snap.Manifest(); mErr != nil {
		app.log.Warn("on-demand par2: cannot verify without the manifest; post-processing will run a full par2 verify instead",
			"job", jobID, "err", mErr)
	} else {
		needsRecovery, reason = par2NeedsRecovery(dir, m, snap.Progress(), app.log, parseOpts)
	}
	if !needsRecovery {
		app.log.Info("on-demand par2: verified clean, skipping recovery volumes", "job", jobID)
		// Log rather than discard: the only failure DiscardDeferredPar2 can
		// still return is the job being genuinely gone (ErrNotFound), and
		// silently swallowing that would mean a job removed mid-verification
		// leaves no trace of why its recovery volumes were never marked
		// FetchNever.
		//
		// DiscardDeferredPar2 marks the fetch policy on JobProgress, which is
		// permanently resident (docs/queue-lifecycle.md), and makes no store
		// call of its own (see its doc comment) — it neither needs the
		// manifest nor can fail on residency, matching SetPar2ReleaseReason.
		// The only failure left is the job being genuinely absent
		// (ErrNotFound), which cannot happen here: jobID was just resolved
		// to snap above.
		if err := app.queue.DiscardDeferredPar2(jobID); err != nil {
			app.log.Warn("on-demand par2: discarding the deferred volumes did not fully succeed",
				"job", jobID, "err", err)
		}
		return false
	}
	if err := app.queue.SetPar2ReleaseReason(jobID, reason); err != nil {
		app.log.Warn("on-demand par2: could not record the release reason",
			"job", jobID, "err", err)
	}
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
func par2NeedsRecovery(dir string, m *queue.Manifest, p *queue.JobProgress, log *slog.Logger, parseOpts par2.ParseOptions) (needsRecovery bool, reason string) {
	sets, err := par2.FindPar2Files(dir, parseOpts)
	if err != nil || len(sets) == 0 {
		reason := "no usable par2 index found to verify against"
		if err != nil {
			reason = fmt.Sprintf("no usable par2 index found (err: %v)", err)
		}
		log.Info("on-demand par2: no usable par2 index to verify against; fetching recovery volumes",
			"dir", dir, "err", err)
		return true, reason
	}
	assembled := make([]par2.AssembledFile, m.NumFiles())
	for fi := range m.NumFiles() {
		name := m.FileSubject(fi)
		if fn := p.FileFilename(fi); fn != "" {
			name = fn
		}
		assembled[fi] = par2.AssembledFile{
			FileName: name,
			CRC32:    p.FileAssembledCRC32(fi),
			FileSize: m.FileBytes(fi),
		}
	}
	r := par2.VerifyCRCsWithOptions(assembled, sets, log, parseOpts)

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

// hasFailedArticle reports whether any article in jf permanently failed
// (exhausted retries on all servers). Such a file can still reach the
// "complete" state — the assembler fires OnFileComplete once every article
// is Done-or-Failed, leaving gaps in the written file rather than blocking
// the job forever on data that will never arrive.
func hasFailedArticle(m *queue.Manifest, p *queue.JobProgress, fileIdx int) bool {
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
	started, err := app.queue.SetPostProcStarted(jobID)
	if err == nil && started {
		// Close any open assembler file handles for this job so post-processing
		// operations (Par2 repair, unpack, cleanup) don't trigger NFS silly-rename
		// (.nfsXXXX) artifacts on open files.
		if err := app.assembler.CloseJobHandles(context.Background(), jobID); err != nil {
			app.log.Warn("maybeFinalize: failed to close assembler job handles", "job", jobID, "err", err)
		}
		// Force an immediate queue save so the PostProc=true flag survives
		// a crash. Without this, a crash between job completion and the
		// next periodic checkpoint (~30s) would lose the flag, causing
		// articles to be re-downloaded on restart instead of entering
		// post-processing via crash recovery.
		adminDir := app.config.GetGeneral().AdminDir
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

func (app *Application) enqueuePostProc(job *queue.Job, failMsg string) {
	// Release cached file info for this job; the assembler no longer
	// needs it, and keeping it around leaks memory across many downloads.
	app.pipeline.forgetJob(job.ID)

	snap := app.config.Snapshot()
	gen := &snap.General
	downloadDirBase := gen.DownloadDir
	completeDir := gen.CompleteDir
	categories := snap.Categories
	sanitize := snap.Downloads.SanitizeOptions()
	downloadDir := filepath.Join(downloadDirBase, job.Name)

	// Log the handoff from download → postproc. This is the "entering
	// postproc" bookend; processJob logs the "exiting" bookend.
	var dlDuration time.Duration
	if !job.Progress().DownloadStarted().IsZero() {
		dlDuration = job.Progress().DownloadFinished().Sub(job.Progress().DownloadStarted())
	}
	app.log.Info("postproc: job entering pipeline",
		"job", job.ID,
		"name", job.Name,
		"category", job.Category,
		"download_dir", downloadDir,
		"download_duration", dlDuration.Round(time.Second),
		"total_bytes", job.TotalBytes(),
		"failed_bytes", job.Progress().FailedBytes(),
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
	// When du != nil, Wait() is executed inside an asynchronous worker
	// goroutine on app.wg so the completion event consumer (watchCompletions)
	// never blocks waiting for disk unpacking to finish.
	du := app.duOrch.collect(job.ID)

	dispatch := func(duResults map[string]directunpack.SuccessSet, duFailures map[string]directunpack.FailedSet, duSkipped map[string]directunpack.SkippedSet) {
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
			dispatch(duResults, duFailures, duSkipped)
		})
		return
	}
	dispatch(nil, nil, nil)
}

// SetQuickCheckEnabled enables or disables the CRC pre-verify pass at runtime
// without restarting. Takes effect for the next job that enters post-processing.

// rebuildJobFromNZB reconstructs a queue.Job for a history entry by
// re-parsing the gzipped NZB backup recorded on it.
//
// This is where the article message-IDs come back. They are not in SQLite —
// job_files holds per-file metadata and a per-article bitmap, never the
// <id@host> strings a BODY command needs — and the job's manifest was
// unlinked when it finalized. The NZB is the only remaining copy, which is
// why the backup's name is recorded on the entry.
//
// The rebuilt job takes the entry's ID so its retained progress, its
// incomplete directory, and any later history entry all still line up.
func (app *Application) rebuildJobFromNZB(entry *history.Entry) (*queue.Job, error) {
	if entry.NZBBackup == "" {
		return nil, fmt.Errorf("app: retry %s: no NZB backup was recorded for this job", entry.NzoID)
	}
	adminDir := app.config.GetGeneral().AdminDir
	// filepath.Base defends against a stored value containing separators;
	// the column is written by us but is not a trust boundary we control
	// once the database is on disk.
	path := filepath.Join(adminDir, "nzb", filepath.Base(entry.NZBBackup))

	f, err := os.Open(path) //nolint:gosec // path is adminDir-rooted with the basename taken above
	if err != nil {
		return nil, fmt.Errorf("app: retry %s: open NZB backup %q: %w", entry.NzoID, entry.NZBBackup, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("app: retry %s: read NZB backup %q: %w", entry.NzoID, entry.NZBBackup, err)
	}
	defer func() { _ = gz.Close() }()

	parsed, err := nzb.Parse(gz)
	if err != nil {
		return nil, fmt.Errorf("app: retry %s: parse NZB backup %q: %w", entry.NzoID, entry.NZBBackup, err)
	}

	pp := types.PPInherit
	if entry.PP != "" {
		if n, convErr := strconv.Atoi(entry.PP); convErr == nil {
			pp = n
		}
	}
	job, err := BuildIngestJob(app.config, parsed, entry.NzbName, types.FetchOptions{
		NzbName:  entry.Name,
		Category: entry.Category,
		Script:   entry.Script,
		Password: entry.Password,
		PP:       pp,
	}, app.log)
	if err != nil {
		return nil, fmt.Errorf("app: retry %s: %w", entry.NzoID, err)
	}
	job.ID = entry.NzoID
	job.NZBBackup = entry.NZBBackup
	return job, nil
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

	job, err := app.rebuildJobFromNZB(entry)
	if err != nil {
		return err
	}
	if store := app.queue.Store(); store != nil {
		applied, rErr := store.RestoreRetryProgress(ctx, job)
		if rErr != nil {
			return fmt.Errorf("app: retry %s: restore retained progress: %w", jobID, rErr)
		}
		if !applied {
			app.log.Info("no usable retained progress for retry; downloading from scratch",
				"job", jobID)
		}
	}
	job.ResetForRetry()
	if err := app.queue.Add(job); err != nil {
		return err
	}
	// Deliberately Delete rather than deleteHistoryEntries: the job is going
	// back into the queue under the same ID and the same NZBBackup, and if
	// it fails again it will finalize into a new history entry naming that
	// same file. Removing the backup here would make the second failure
	// unretryable. The retained per-file progress does go, because Add has
	// just written fresh job_files rows that supersede it.
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

func failMsgForJob(job *queue.Job) string {
	p := job.Progress()

	// Promoted scalars and JobProgress, never job.Manifest(): this runs from
	// the startup recovery walk over Queue.Snapshot(), where a job may have
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
	switch state := job.RepairState(); state {
	case queue.RepairIntact, queue.RepairPossible, queue.RepairUnknown:
		// RepairIntact: only par2 failed, so there is nothing to repair and
		// the job merely cannot be verified. RepairPossible: within capacity.
		// RepairUnknown: par2 present but unrecognized, so capacity is
		// unmeasured rather than absent. All three proceed to
		// post-processing.
		return ""
	case queue.RepairBeyondCapacity:
		// Capacity may be understated if some plainly-named par2 file also
		// carries slices, so this errs toward aborting.
		recoveryMB := float64(job.RecoveryBytes()) / (1024 * 1024)
		return fmt.Sprintf(
			"Aborted: %.1f MB failed, exceeds repair capacity of %.1f MB (%d recovery volumes). Job is beyond repair",
			failedMB, recoveryMB, job.RecoveryFiles(),
		)
	case queue.RepairNoCapacity:
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
		if state.Hopeless() {
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
	name := queue.UniqueName(base, func(candidate string) bool {
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
