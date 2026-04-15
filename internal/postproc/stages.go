// Package postproc implements the post-processing orchestrator for SABnzbd-Go.
// It mirrors the Python sabnzbd/postproc.py behaviour: a single-worker pipeline
// with a fast queue (for DirectUnpack-assisted jobs) and a slow queue, running
// registered Stage implementations in order for each job.
package postproc

import (
	"context"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// Stage is the interface every post-processing stage must implement.
// Stages are registered once at construction time and run in that order
// for every job. A stage that returns an error does NOT abort the pipeline;
// the error is captured in StageLog and subsequent stages still run.
type Stage interface {
	// Name returns a short, stable identifier used in log output and the
	// StageLog.  It should be lowercase with no spaces (e.g. "repair",
	// "unpack", "sort").
	Name() string

	// Run executes the stage.  The supplied ctx is cancelled when the
	// PostProcessor is stopped; stages MUST respect it and return promptly.
	// Returning a non-nil error records the failure but does not halt the
	// pipeline.
	Run(ctx context.Context, job *Job) error
}

// DirectUnpackState carries the information the orchestrator needs to decide
// whether a job belongs on the fast queue. Steps 5.2+ will embed richer state
// here; for now the single bool is sufficient for routing.
type DirectUnpackState struct {
	// Active is true while a DirectUnpack goroutine is still running for
	// this job, or true when at least one set was successfully unpacked
	// (mirroring the Python condition:
	//   nzo.direct_unpacker.success_sets or not nzo.direct_unpacker.killed).
	Active bool
}

// Job is the post-processing unit of work.  It wraps the download-queue Job
// with post-proc-specific state.  The queue.Job must not be mutated here;
// stages accumulate their results into the fields below.
type Job struct {
	// Queue is the source job from the download queue. Read-only for stages.
	Queue *queue.Job

	// DirectUnpack is non-nil when the job was routed via the fast queue.
	// Nil for slow-queue jobs.
	DirectUnpack *DirectUnpackState

	// StageLog accumulates one entry per stage, in execution order.
	StageLog []StageLogEntry

	// FailMsg is set by a stage that considers the whole job failed.
	// The orchestrator does not inspect it; it is preserved for history/UI.
	FailMsg string

	// ParError and UnpackError are set by the repair and unpack stages
	// respectively (Steps 5.2/5.3).  Defined here so all stages can read
	// and set them without a type assertion.
	ParError    bool
	UnpackError bool
}

// StageLogEntry records the outcome of a single stage execution.
type StageLogEntry struct {
	// Stage is the Stage.Name() value.
	Stage string

	// Started is the wall-clock time the stage began.
	Started time.Time

	// Elapsed is how long the stage took.
	Elapsed time.Duration

	// Err is the error returned by Stage.Run, or nil on success.
	Err error

	// Lines holds any structured log lines emitted by the stage.
	// Stages append to this slice via the helper below; the orchestrator
	// passes a *StageLogEntry to each stage (Steps 5.2+).
	Lines []string
}
