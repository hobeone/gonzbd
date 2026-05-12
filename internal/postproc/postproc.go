package postproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
)

// Options configures a PostProcessor at construction time.
type Options struct {
	// Stages is the ordered list of post-processing stages.  They run in
	// slice order for every job.  An empty slice is valid (no-op pipeline).
	Stages []Stage

	// OnEmpty is called (in the worker goroutine) when the queue drains to
	// empty after processing a job.  Mirrors Python's handle_empty_queue.
	// May be nil.
	OnEmpty func()

	// OnJobDone is called (in the worker goroutine) exactly once per finished
	// job, with the full StageLog populated.  May be nil.
	OnJobDone func(*Job)

	// StatusUpdater is called to update the persistent status of the job in
	// the active queue. Usually maps to queue.SetStatus.
	StatusUpdater func(string, constants.Status)

	// Logger is the structured logger.  Defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// PostProcessor is the post-processing orchestrator.  It owns a single worker
// goroutine that dequeues jobs from the ppQueue and runs each
// registered Stage in order.
//
// Use New to construct; Start to launch the worker; Stop to shut it down
// gracefully.  All public methods are safe for concurrent use.
type PostProcessor struct {
	stages        []Stage
	onEmpty       func()
	onJobDone     func(*Job)
	statusUpdater func(string, constants.Status)
	log           *slog.Logger

	q *ppQueue

	// pause / resume coordination.
	pauseMu sync.Mutex
	paused  bool
	// resumeC is a channel that is closed when the processor resumes.
	// It is replaced with a fresh channel on each Pause call so that
	// subsequent Pause/Resume cycles work correctly.  Always read resumeC
	// under pauseMu and copy to a local before releasing.
	resumeC chan struct{}

	// workerCtx / workerCancel drive the worker lifecycle.
	workerCtx    context.Context //nolint:containedctx // intentional: worker context lives in struct
	workerCancel context.CancelFunc

	// wg tracks the worker goroutine so Stop can wait for it.
	wg sync.WaitGroup

	// busy is true while a job's stages are executing.
	// currentJobID is the ID of the in-flight job (empty when not busy).
	// started guards against double Start calls.
	// All three are guarded by busyMu so Has can atomically observe the
	// "queued-or-running" set.
	busyMu       sync.Mutex
	busy         bool
	started      bool
	currentJobID string

	// history tracks all completed jobs for the UI.
	historyMu sync.RWMutex
	history   []*Job
}

// New constructs a PostProcessor from opts.  It does not start the worker;
// call Start for that.
func New(opts Options) *PostProcessor {
	lg := opts.Logger
	if lg == nil {
		lg = slog.Default()
	}
	log := lg.With("component", "postproc")
	return &PostProcessor{
		stages:        opts.Stages,
		onEmpty:       opts.OnEmpty,
		onJobDone:     opts.OnJobDone,
		statusUpdater: opts.StatusUpdater,
		log:           log,
		q:             newPPQueue(),
		resumeC:       make(chan struct{}),
	}
}

// ErrAlreadyStarted is returned by Start when the worker is already running.
var ErrAlreadyStarted = errors.New("postproc: already started")

// Start launches the worker goroutine.  ctx is the application-level context;
// the worker also stops when Stop is called.  Returns ErrAlreadyStarted if
// the worker is already running.
func (p *PostProcessor) Start(ctx context.Context) error {
	p.busyMu.Lock()
	if p.started {
		p.busyMu.Unlock()
		return ErrAlreadyStarted
	}
	p.started = true
	p.busyMu.Unlock()

	p.workerCtx, p.workerCancel = context.WithCancel(ctx)
	p.wg.Go(func() {
		p.run()
	})
	return nil
}

// Stop signals the worker to exit and waits until it has.  Idempotent.
func (p *PostProcessor) Stop() error {
	if p.workerCancel != nil {
		p.workerCancel()
	}
	// Unblock the worker if it is waiting on a resume signal so it can
	// observe the cancelled context and exit.
	p.signalResume()
	p.wg.Wait()
	return nil
}

// Process enqueues job for post-processing.
func (p *PostProcessor) Process(job *Job) {
	p.log.Info("postproc: enqueuing job", "job", job.Queue.ID)
	p.q.Push(job)
}

// Pause halts processing after the current job finishes.  Safe to call
// multiple times; subsequent calls are no-ops.
func (p *PostProcessor) Pause() {
	p.pauseMu.Lock()
	defer p.pauseMu.Unlock()
	if !p.paused {
		p.paused = true
		// Replace resumeC so the next Resume closes a fresh channel.
		p.resumeC = make(chan struct{})
		p.log.Info("postproc: paused")
	}
}

// Resume continues processing after a Pause.  Safe to call when not paused.
func (p *PostProcessor) Resume() {
	p.pauseMu.Lock()
	defer p.pauseMu.Unlock()
	if !p.paused {
		return
	}
	p.paused = false
	close(p.resumeC)
	p.log.Info("postproc: resumed")
}

// signalResume unblocks a paused worker without changing the paused flag.
// Used by Stop so that a paused worker can observe ctx cancellation.
func (p *PostProcessor) signalResume() {
	p.pauseMu.Lock()
	defer p.pauseMu.Unlock()
	select {
	case <-p.resumeC:
		// already closed
	default:
		close(p.resumeC)
	}
}

// Cancel removes job with jobID from the queue.  If the job is
// currently being processed its context is already managed by Stop/workerCtx;
// stages must respect ctx.Done() for cancellation during execution.
// Returns true if the job was found and removed from the queue.
func (p *PostProcessor) Cancel(jobID string) bool {
	return p.q.Cancel(jobID)
}

// Empty returns true when the queue is empty and no job is currently being
// processed.
func (p *PostProcessor) Empty() bool {
	p.busyMu.Lock()
	busy := p.busy
	p.busyMu.Unlock()
	return !busy && p.q.Empty()
}

// Has reports whether a job with jobID is either pending in the queue or
// currently being processed by the worker. Callers use this as a
// deduplication gate when bypassing the regular handoff path (e.g. the
// Application startup rescan for jobs whose PostProc flag persisted
// across a crash).
func (p *PostProcessor) Has(jobID string) bool {
	p.busyMu.Lock()
	current := p.currentJobID
	p.busyMu.Unlock()
	if current == jobID {
		return true
	}
	return p.q.Has(jobID)
}

// History returns a deep-copy snapshot of all jobs that have passed through
// the post-processor (including currently in-flight jobs). The copies prevent
// data races between the worker mutating StageLog and callers reading the
// snapshot.
func (p *PostProcessor) History() []*Job {
	p.historyMu.RLock()
	defer p.historyMu.RUnlock()
	out := make([]*Job, len(p.history))
	for i, j := range p.history {
		cp := *j
		cp.StageLog = make([]StageLogEntry, len(j.StageLog))
		copy(cp.StageLog, j.StageLog)
		out[i] = &cp
	}
	return out
}

// run is the worker goroutine body.
func (p *PostProcessor) run() {
	for {
		p.setBusy(false)

		// Check for stop before blocking.
		select {
		case <-p.workerCtx.Done():
			return
		default:
		}

		// Wait for a job, respecting pause.
		job, ok := p.popWithPause()
		if !ok {
			// ctx cancelled.
			return
		}

		// setBusyWithJob was already called inside popWithPause to
		// eliminate the race window between pop and busy-set.
		p.processJob(job)

		// If the worker context was cancelled (shutdown), the job was only
		// partially processed. Skip onJobDone so it remains in the active
		// queue. On the next startup, crash recovery will find it with
		// PostProc=true and re-enqueue it for post-processing.
		if p.workerCtx.Err() != nil {
			p.setBusyWithJob(false, "")
			p.log.Info("postproc: shutdown interrupted job, preserving for recovery",
				"job", job.Queue.ID)
			return
		}

		p.addHistory(job)
		p.setBusyWithJob(false, "")

		if p.onJobDone != nil {
			p.onJobDone(job)
		}

		// If the queue is empty after processing this job, call OnEmpty.
		if p.q.Empty() && p.onEmpty != nil {
			p.onEmpty()
		}
	}
}

// popWithPause wraps q.Pop and inserts a pause-check loop.
// Returns (job, true) on success, (nil, false) when ctx is done.
func (p *PostProcessor) popWithPause() (*Job, bool) {
	for {
		// Grab the current resumeC before checking paused, so we don't
		// race between paused==true and the channel being closed.
		p.pauseMu.Lock()
		paused := p.paused
		resumeC := p.resumeC
		p.pauseMu.Unlock()

		if paused {
			p.log.Debug("postproc: worker paused, waiting for resume")
			select {
			case <-p.workerCtx.Done():
				return nil, false
			case <-resumeC:
				// Loop back to re-check paused state.
				continue
			}
		}

		// Not paused — try to pop.
		job, ok := p.q.Pop(p.workerCtx)
		if !ok {
			return nil, false
		}

		// Re-check pause after Pop returns: a Pause may have landed while we
		// were blocked in Pop, or between paused==false and the Pop return.
		// If paused, put the job back at the head and loop to wait on resumeC.
		p.pauseMu.Lock()
		stillPaused := p.paused
		p.pauseMu.Unlock()
		if stillPaused {
			p.q.PushHead(job)
			continue
		}

		// Set busy atomically before returning so Has()/Empty() never
		// see the intermediate state (queue empty, not busy).
		p.setBusyWithJob(true, job.Queue.ID)
		return job, true
	}
}

// processJob runs all registered stages in order for job.
// Stage errors are recorded but do not abort the pipeline.
//
// If the job already carries a FailMsg (e.g. "beyond repair" from the
// download health gate), all processing stages are skipped. The job
// still flows through to history so the user sees the failure.
func (p *PostProcessor) processJob(job *Job) {
	p.log.Info("postproc: processing job", "job", job.Queue.ID, "name", job.Queue.Name)

	// Build a synthetic "download" stage that captures the files present
	// in the download directory before any stages run. This gives the
	// history UI a clear view of the starting state for debugging.
	var dlElapsed time.Duration
	dlStarted := job.Queue.DownloadStarted
	if dlStarted.IsZero() {
		dlStarted = time.Now()
	}
	if !job.Queue.DownloadFinished.IsZero() {
		dlElapsed = job.Queue.DownloadFinished.Sub(dlStarted)
	}
	dlLines := buildDownloadFileList(job)
	job.StageLog = append(job.StageLog, StageLogEntry{
		Stage:   "download",
		Started: dlStarted,
		Elapsed: dlElapsed,
		Lines:   dlLines,
	})

	if job.FailMsg != "" {
		p.log.Warn("postproc: skipping all stages — job already failed",
			"job", job.Queue.ID,
			"reason", job.FailMsg,
		)
		job.StageLog = append(job.StageLog, StageLogEntry{
			Stage:   "skipped",
			Started: time.Now(),
			Lines:   []string{"Post-processing skipped: " + job.FailMsg},
		})
		return
	}

	var failedAt = -1 // index of first failed stage, or -1

	for i, stage := range p.stages {
		// PP enforcement (M1): skip stages above the job's post-processing
		// level. SABnzbd PP levels are cumulative:
		//   0 = download only (skip repair + unpack)
		//   1 = +repair (par2 verify/repair)
		//   2 = +unpack (also does repair)
		//   3 = +delete (repair + unpack + cleanup)
		// Quickcheck and repair require PP ≥ 1; unpack requires PP ≥ 2.
		// Other stages (deobfuscate, sort, finalize, script) always run.
		if shouldSkipForPP(stage.Name(), job.Queue.PP) {
			p.log.Info("postproc: skipping stage (PP level)",
				"stage", stage.Name(),
				"job", job.Queue.ID,
				"pp", job.Queue.PP,
			)
			job.StageLog = append(job.StageLog, StageLogEntry{
				Stage:   stage.Name(),
				Started: time.Now(),
				Lines:   []string{fmt.Sprintf("Skipped: PP=%d (stage requires higher PP level)", job.Queue.PP)},
			})
			continue
		}

		if p.statusUpdater != nil {
			var status constants.Status
			switch stage.Name() {
			case "repair":
				status = constants.StatusVerifying
			case "unpack":
				status = constants.StatusExtracting
			case "finalize":
				status = constants.StatusMoving
			default:
				status = constants.StatusRunning
			}
			p.statusUpdater(job.Queue.ID, status)
		}

		entry := StageLogEntry{
			Stage:   stage.Name(),
			Started: time.Now(),
		}

		err := stage.Run(p.workerCtx, job)
		entry.Elapsed = time.Since(entry.Started)
		entry.Err = err

		// Capture any tool output lines the stage deposited.
		if len(job.OutputLines) > 0 {
			entry.Lines = append(entry.Lines, job.OutputLines...)
			job.OutputLines = job.OutputLines[:0]
		}

		if err != nil {
			p.log.Warn("postproc: stage failed, aborting pipeline",
				"stage", stage.Name(),
				"job", job.Queue.ID,
				"err", err,
			)
			job.StageLog = append(job.StageLog, entry)
			failedAt = i
			break
		}

		p.log.Info("postproc: stage done",
			"stage", stage.Name(),
			"job", job.Queue.ID,
			"elapsed", entry.Elapsed,
		)
		job.StageLog = append(job.StageLog, entry)

		// If the worker context was cancelled mid-stage, stop running further
		// stages — the stage itself should have returned early, but we don't
		// force it to.
		select {
		case <-p.workerCtx.Done():
			p.log.Info("postproc: worker context cancelled, aborting remaining stages",
				"job", job.Queue.ID,
			)
			return
		default:
		}
	}

	// Record remaining stages as skipped if the pipeline was aborted.
	if failedAt >= 0 {
		failedName := p.stages[failedAt].Name()
		for _, stage := range p.stages[failedAt+1:] {
			job.StageLog = append(job.StageLog, StageLogEntry{
				Stage:   stage.Name(),
				Started: time.Now(),
				Lines:   []string{fmt.Sprintf("Skipped: %s stage failed", failedName)},
			})
		}
	}

	p.log.Info("postproc: job complete",
		"job", job.Queue.ID,
		"stages", len(job.StageLog),
		"fail_msg", job.FailMsg,
	)

	// Build a synthetic "summary" stage for the history/UI. This gives the
	// expanded history row a final card showing the pipeline at a glance.
	var summaryLines []string
	var pipelineStart, pipelineEnd time.Time
	for _, sl := range job.StageLog {
		// Track overall pipeline start/end from individual stage timings.
		if pipelineStart.IsZero() || sl.Started.Before(pipelineStart) {
			pipelineStart = sl.Started
		}
		stageEnd := sl.Started.Add(sl.Elapsed)
		if stageEnd.After(pipelineEnd) {
			pipelineEnd = stageEnd
		}
		symbol := "✓"
		detail := fmt.Sprintf("%.1fs", sl.Elapsed.Seconds())
		if sl.Err != nil {
			symbol = "✗"
			detail += " — " + sl.Err.Error()
		}
		summaryLines = append(summaryLines, fmt.Sprintf("%s %s (%s)", symbol, sl.Stage, detail))
	}
	totalDuration := pipelineEnd.Sub(pipelineStart)
	finalStatus := "Completed"
	if job.FailMsg != "" || job.ParError || job.UnpackError {
		finalStatus = "Failed"
	}
	header := fmt.Sprintf("Pipeline %s in %.1fs", finalStatus, totalDuration.Seconds())
	if job.FinalDir != "" {
		header += " → " + job.FinalDir
	}
	summaryLines = append([]string{header}, summaryLines...)

	// Append the final file listing so the user can see what ended up
	// in the output directory.
	if finalFiles := buildFinalFileList(job); len(finalFiles) > 0 {
		summaryLines = append(summaryLines, "") // blank separator
		summaryLines = append(summaryLines, finalFiles...)
	}

	job.StageLog = append(job.StageLog, StageLogEntry{
		Stage:   "summary",
		Started: pipelineEnd,
		Elapsed: totalDuration,
		Lines:   summaryLines,
	})
}

func (p *PostProcessor) setBusy(v bool) {
	p.busyMu.Lock()
	p.busy = v
	p.busyMu.Unlock()
}

// setBusyWithJob updates busy and currentJobID atomically. Used by the
// worker around each processJob call so Has can observe the in-flight
// job ID without racing the busy flag.
func (p *PostProcessor) setBusyWithJob(v bool, jobID string) {
	p.busyMu.Lock()
	p.busy = v
	p.currentJobID = jobID
	p.busyMu.Unlock()
}

func (p *PostProcessor) addHistory(job *Job) {
	p.historyMu.Lock()
	p.history = append(p.history, job)
	// Cap in-memory history to prevent unbounded growth in long-running
	// daemons. The authoritative history lives in the SQLite DB; this
	// slice is only for the API's "recent postproc" view.
	const maxHistory = 1000
	if len(p.history) > maxHistory {
		// Drop the oldest entries and nil out for GC.
		excess := len(p.history) - maxHistory
		for i := range excess {
			p.history[i] = nil
		}
		p.history = p.history[excess:]
	}
	p.historyMu.Unlock()
}

// shouldSkipForPP returns true if the named stage should be skipped because
// the job's PP level is too low. SABnzbd PP levels are cumulative:
//
//	0 = download only (no repair, no unpack)
//	1 = +repair (par2 verify/repair)
//	2 = +unpack (includes repair)
//	3 = +delete (includes repair + unpack + archive cleanup)
//
// Stages always run: quickcheck (just logging), deobfuscate, sample, sort,
// finalize, script. Stages gated by PP: repair (≥1), unpack (≥2).
func shouldSkipForPP(stageName string, pp int) bool {
	switch stageName {
	case "quickcheck", "repair":
		return pp < 1
	case "unpack":
		return pp < 2
	default:
		return false
	}
}
