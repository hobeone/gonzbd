// Package postproc implements the post-processing orchestrator for gonzbd.
// It mirrors the Python sabnzbd/postproc.py behaviour: a single-worker pipeline
// with a fast queue (for DirectUnpack-assisted jobs) and a slow queue, running
// registered Stage implementations in order for each job.
package postproc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/queue"
)

// Stage is the interface every post-processing stage must implement.
// Stages are registered once at construction time and run in that order
// for every job. Stage errors are recorded but the pipeline continues; each
// stage self-gates based on job flags (ParError, UnpackError).
type Stage interface {
	// Name returns a short, stable identifier used in log output and the
	// StageLog.  It should be lowercase with no spaces (e.g. "repair",
	// "unpack", "sort").
	Name() string

	// Run executes the stage.  The supplied ctx is cancelled when the
	// PostProcessor is stopped; stages MUST respect it and return promptly.
	// Returning a non-nil error records the failure in the StageLog but
	// does NOT abort the pipeline; subsequent stages still run.
	Run(ctx context.Context, job *Job) error
}

// Job is the post-processing unit of work.  It wraps the download-queue Job
// with post-proc-specific state.  The queue.Job must not be mutated here;
// stages accumulate their results into the fields below.
type Job struct {
	// Queue is the source job from the download queue. Read-only for stages.
	Queue *queue.Job

	// DownloadDir is the absolute path where the assembler wrote the job's
	// files — the working directory for in-place stages (par2, unpack,
	// deobfuscate, pre-sort). Mirrors Python's nzo.download_path. Must be
	// set by the caller before the job is pushed to the PostProcessor.
	DownloadDir string

	// FinalDir is the absolute path where the job's files should end up
	// after all post-processing stages have finished. Usually a sub-path
	// of the complete directory named after the job name.
	FinalDir string

	// Sanitize defines the naming replacement options for this job.
	Sanitize fsutil.SanitizeOptions

	// StageLog accumulates one entry per stage, in execution order.
	StageLog []StageLogEntry

	// FailMsg is set by a stage that considers the whole job failed.
	// The orchestrator does not inspect it; it is preserved for history/UI.
	FailMsg string

	// ParError and UnpackError are set by the repair and unpack stages
	// respectively (Steps 5.2/5.3).  Downstream stages (sort, script) read
	// them; scripts in particular surface these via the pp_status argv and
	// SAB_PP_STATUS env var.
	ParError    bool
	UnpackError bool

	// QuickCheckPassed is set by the quickcheck stage when ALL par2-tracked
	// files have matching CRC32 values. When true, the repair stage skips
	// the expensive par2 subprocess since verification already confirmed
	// file integrity.
	QuickCheckPassed bool

	// NeedRequeue is set by the repair stage when par2 reports that the
	// job needs additional recovery blocks ("You need N more blocks") or
	// the main par2 file is corrupt/missing. When true, processJob stops
	// the pipeline after repair and the caller should push the job back
	// to the download queue to fetch more par2 volumes.
	// Matches SABnzbd's readd path in par2_repair / par2cmdline_verify.
	NeedRequeue bool

	// RequeueBlocksNeeded is the number of additional recovery blocks
	// par2 requires. Only meaningful when NeedRequeue is true.
	RequeueBlocksNeeded int

	// RequeueReason describes why the job needs requeue (for logging).
	RequeueReason string

	// OutputLines is a scratch buffer that stages populate with tool output
	// lines (e.g. par2 stdout, unrar output). processJob moves these into
	// StageLogEntry.Lines after each stage runs and clears the buffer.
	// Stages should append to this slice rather than writing to Lines
	// directly, since StageLogEntry is created by the orchestrator.
	OutputLines []string

	// ConsumedFiles lists absolute paths of files consumed by repair/join
	// operations (par2 volumes, joinable split files). Extension cleanup
	// skips these to prevent deletion of files needed for recovery.
	ConsumedFiles map[string]struct{}

	// Par2Renames maps par2's canonical filename → actual on-disk filename.
	// Populated from par2's "is a match for" output during repair. Used by
	// downstream stages (deobfuscate, sort) to apply par2-discovered renames.
	// Matches SABnzbd's nzo.renamed_file(renames) behavior.
	Par2Renames map[string]string

	// OnOutput is called by stages when a subprocess emits a line of output.
	// The tool parameter identifies the source (e.g. "par2", "unrar", "7z",
	// "script"). May be nil (output is still captured in OutputLines).
	OnOutput func(tool, line string) `json:"-"`

	// DirectUnpackSets holds results from DirectUnpack — archive sets that
	// were already extracted during download. The unpack stage skips these
	// sets. Nil when DirectUnpack is disabled or didn't run.
	DirectUnpackSets map[string]directunpack.SuccessSet

	// DirectUnpackFailures holds sets that DirectUnpack attempted but
	// failed to extract, along with the failure reason. These sets will
	// be retried by the normal unpack stage.
	DirectUnpackFailures map[string]directunpack.FailedSet
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

// stageLogJSON is the JSON-safe representation of StageLogEntry.
// The error interface is serialized as a *string because
// json.Marshal(error) produces "{}" (empty object), which the
// history API deserializer cannot parse as a string.
type stageLogJSON struct {
	Stage   string        `json:"Stage"`
	Started time.Time     `json:"Started"`
	Elapsed time.Duration `json:"Elapsed"`
	Err     *string       `json:"Err"`
	Lines   []string      `json:"Lines"`
}

// MarshalJSON implements json.Marshaler for StageLogEntry.
func (e StageLogEntry) MarshalJSON() ([]byte, error) {
	j := stageLogJSON{
		Stage:   e.Stage,
		Started: e.Started,
		Elapsed: e.Elapsed,
		Lines:   e.Lines,
	}
	if e.Err != nil {
		s := e.Err.Error()
		j.Err = &s
	}
	return json.Marshal(j)
}
