package postproc

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/constants"
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
	// Both are guarded by busyMu so Has can atomically observe the
	// "queued-or-running" set.
	busyMu       sync.Mutex
	busy         bool
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

// Start launches the worker goroutine.  ctx is the application-level context;
// the worker also stops when Stop is called.  Returns an error only when the
// worker is already running.
func (p *PostProcessor) Start(ctx context.Context) error {
	p.workerCtx, p.workerCancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go p.run()
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
	if !p.paused {
		p.pauseMu.Unlock()
		return
	}
	p.paused = false
	ch := p.resumeC
	p.pauseMu.Unlock()
	// Close outside the lock to avoid holding it while waking the worker.
	closeOnce(ch)
	p.log.Info("postproc: resumed")
}

// signalResume unblocks a paused worker without changing the paused flag.
// Used by Stop so that a paused worker can observe ctx cancellation.
func (p *PostProcessor) signalResume() {
	p.pauseMu.Lock()
	ch := p.resumeC
	p.pauseMu.Unlock()
	closeOnce(ch)
}

// closeOnce closes ch if it is not already closed.
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
		// already closed
	default:
		close(ch)
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

// History returns a snapshot of all jobs that have passed through the
// post-processor (including currently in-flight jobs).
func (p *PostProcessor) History() []*Job {
	p.historyMu.RLock()
	defer p.historyMu.RUnlock()
	out := make([]*Job, len(p.history))
	copy(out, p.history)
	return out
}

// run is the worker goroutine body.
func (p *PostProcessor) run() {
	defer p.wg.Done()

	prevJobDone := false
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

		// If a job just completed and both queues are now empty, call OnEmpty.
		if prevJobDone && p.q.Empty() && p.onEmpty != nil {
			p.onEmpty()
		}

		p.setBusyWithJob(true, job.Queue.ID)
		p.addHistory(job)
		p.processJob(job)
		p.setBusyWithJob(false, "")

		if p.onJobDone != nil {
			p.onJobDone(job)
		}
		prevJobDone = true
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

		// Re-check pause: Resume may have been called just after Pop returned
		// but we honour the job we already pulled.
		return job, true
	}
}

// processJob runs all registered stages in order for job.
// Stage errors are recorded but do not abort the pipeline.
func (p *PostProcessor) processJob(job *Job) {
	p.log.Info("postproc: processing job", "job", job.Queue.ID, "name", job.Queue.Name)

	for _, stage := range p.stages {
		if p.statusUpdater != nil {
			var status constants.Status
			switch stage.Name() {
			case "repair":
				status = constants.StatusRepairing
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

		if err != nil {
			p.log.Warn("postproc: stage error (continuing)",
				"stage", stage.Name(),
				"job", job.Queue.ID,
				"err", err,
			)
		} else {
			p.log.Info("postproc: stage done",
				"stage", stage.Name(),
				"job", job.Queue.ID,
				"elapsed", entry.Elapsed,
			)
		}

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

	p.log.Info("postproc: job complete",
		"job", job.Queue.ID,
		"stages", len(job.StageLog),
		"fail_msg", job.FailMsg,
	)
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
	p.historyMu.Unlock()
}
