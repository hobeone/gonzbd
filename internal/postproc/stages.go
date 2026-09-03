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
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/types"
)

// QuickCheckOutcome is what the quickcheck stage was able to determine about
// a job's file CRCs. It replaces the QuickCheckRan/QuickCheckPassed pair,
// which could not express its fourth combination and so collapsed two
// unrelated states into one.
//
// The distinction that matters is between "I found nothing wrong" and "I have
// nothing to say". Both used to read as !QuickCheckRan, and the repair
// stage's DirectUnpack shortcut treats a silent quickcheck as consent —
// skipping par2 entirely for a job whose CRCs nothing had verified. Making
// the states exhaustive means a reader has to name which one it means, so
// that collapse cannot be rebuilt by accident.
type QuickCheckOutcome int

const (
	// QuickCheckNotRun means nothing was attempted, because the stage was
	// disabled or found no par2 sets to check against. There is nothing to
	// verify, so DirectUnpack's own signal is the only one available and
	// the repair shortcut may rely on it.
	QuickCheckNotRun QuickCheckOutcome = iota

	// QuickCheckClean means every par2-tracked file's CRC32 matched. Repair can
	// skip the expensive par2 subprocess outright — verification already
	// confirmed integrity.
	QuickCheckClean

	// QuickCheckDamaged means verification ran and found unverifiable or
	// mismatched files. Repair must run regardless of what DirectUnpack
	// reported, which only knows that rarengine could mechanically walk the
	// archive's entries.
	QuickCheckDamaged

	// QuickCheckInconclusive means verification was attempted and could not
	// complete. No claim is being made about the data either way, which is
	// precisely why this must not be treated as QuickCheckNotRun: par2 sets
	// may well exist and nothing has checked them.
	//
	// It is also the default the stage adopts the moment it knows par2 sets
	// exist, narrowing to Clean or Damaged only where verification actually
	// happened (#314). So the causes are "anything that did not earn a
	// verdict", and only one of them involves the manifest — do not go
	// looking for a manifest problem first:
	//   - the par2 scan itself failed, so whether the job has par2 sets is
	//     unknown (the one case set before the default applies);
	//   - par2 sets were found but par2.Assess errored before
	//     verifying any of them;
	//   - the job's manifest was unreadable or described no files, so there
	//     were no expected CRCs to compare against;
	//   - par2 sets were found and no assembled CRC was available for any of
	//     them, so the comparison had nothing on either side.
	QuickCheckInconclusive
)

// AllQuickCheckOutcomes returns every declared outcome, so a test can assert
// that a switch over them handles each one rather than falling through
// silently. Kept in declaration order.
//
// It is hand-written, which on its own would make it a second copy of the
// enum carrying the same defect: a value added to the const block but not
// here is invisible to every loop over it, and every exhaustiveness test
// built on it passes vacuously. TestAllQuickCheckOutcomes_Exhaustive closes
// that loop by parsing the const block itself, the same way
// constants.AllStatuses is pinned (#291).
func AllQuickCheckOutcomes() []QuickCheckOutcome {
	return []QuickCheckOutcome{
		QuickCheckNotRun,
		QuickCheckClean,
		QuickCheckDamaged,
		QuickCheckInconclusive,
	}
}

// String makes the outcome legible in logs and test failures rather than
// printing a bare integer.
func (o QuickCheckOutcome) String() string {
	switch o {
	case QuickCheckNotRun:
		return "not-run"
	case QuickCheckClean:
		return "clean"
	case QuickCheckDamaged:
		return "damaged"
	case QuickCheckInconclusive:
		return "inconclusive"
	default:
		return "unknown"
	}
}

// Stage is the interface every post-processing stage must implement.
// Stages are registered once at construction time and run in that order
// for every job. Stage errors are recorded but the pipeline continues; each
// stage self-gates based on job flags (ParError, UnpackError).
type Stage interface {
	// Name returns a short, stable identifier used in log output and the
	// StageLog.  It should be lowercase with no spaces (e.g. "repair",
	// "unpack", "sort").
	Name() string

	// Run executes the stage.  The supplied ctx is cancelled either when the
	// PostProcessor is stopped, or when this specific job is removed via
	// Cancel while it's being processed; stages MUST respect it and return
	// promptly in both cases. Returning a non-nil error records the failure
	// in the StageLog but does NOT abort the pipeline; subsequent stages
	// still run.
	Run(ctx context.Context, job *Job) error
}

// JobMeta carries the caller-known job metadata that internal/job.Job does
// not carry and that internal/dispatch.Header does not carry either.
// internal/job.Job holds only id/name/policy plus content (job/job.go's own
// doc comment: "id, name and policy are not guarded"); internal/dispatch's
// Header adds category/priority/bytes/added for a queue listing
// (internal/dispatch/registry.go). Neither has room for the NZB-derived and
// config fields this package still needs to render a script's environment:
// the archive password, the origin URL, the configured script name, and the
// original NZB filename.
//
// Deliberately NOT here: the PP level. It used to be carried as a raw int
// alongside job.Job's own Policy, which is exactly the two-writer smell
// Standing Design Rule 2 names — nothing bound the two together, and a
// caller that built job.Job with one PP level and populated this struct with
// another would go undetected. ppFromPolicy derives the integer SAB_PP_STATUS
// needs from job.Job.Policy() instead, so there is one PP fact per job, not
// two that can drift.
//
// The caller (internal/app) populates this before calling Process, the same
// way it already populates DownloadDir/FinalDir/Sanitize below — this is not
// a new pattern, just three more fields of the same kind.
type JobMeta struct {
	// Filename is the original NZB filename, surfaced to scripts as
	// SAB_NZBNAME / ScriptInput.NZBName.
	Filename string
	// Category is the configured category name, surfaced as SAB_CAT.
	Category string
	// Script is the name of the user post-proc script to run, resolved
	// relative to ScriptStage.ScriptDir.
	Script string
	// Password is the archive password extracted from the filename or
	// supplied by the user. Empty if the job is unencrypted.
	Password string //nolint:gosec // G117: NZB archive password, not a credential
	// URL is the origin URL for URL-grabbed NZBs; empty for uploaded or
	// watched-dir NZBs.
	URL string
}

// ppFromPolicy derives the raw upstream PP integer from a Policy — the
// inverse of job.PolicyFromPP. It exists so that everywhere this package
// needs the exact configured level (SAB_PP_STATUS's PPFlags, and the
// "Skipped: PP=%d" log line) reads it from job.Job.Policy() — the one fact
// job.Job was actually built with — rather than from a second,
// independently settable field. See JobMeta's doc comment for the smell
// this replaces.
//
// job.PolicyFromPP is cumulative and saturating (pp<0 -> Policy{}, pp>3 ->
// every field true), so recovering the ladder position from the booleans
// alone is exact for every Policy PolicyFromPP can actually produce: Delete
// implies pp>=3, Unpack implies pp>=2, Repair (PolicyFromPP always sets it
// equal to Verify, at pp>=1) implies pp>=1, else pp==0.
func ppFromPolicy(p job.Policy) int {
	switch {
	case p.Delete:
		return types.PPDelete
	case p.Unpack:
		return types.PPUnpack
	case p.Repair:
		return types.PPVerify
	default:
		return 0
	}
}

// Job is the post-processing unit of work.  It wraps the lifecycle Job with
// post-proc-specific state.  The wrapped *job.Job must not be mutated here;
// stages accumulate their results into the fields below.
type Job struct {
	// Job is the source job from the lifecycle machine. Read-only for
	// stages: it is where Progress()/Manifest()/RepairState() etc. come
	// from, but no stage may call one of its mutators.
	Job *job.Job

	// Meta is the caller-supplied metadata job.Job itself does not carry.
	// See JobMeta's doc comment for why it exists as a separate struct
	// rather than fields on job.Job.
	Meta JobMeta

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

	// QuickCheck is what the quickcheck stage was able to determine about
	// this job's file CRCs. See QuickCheckOutcome.
	QuickCheck QuickCheckOutcome

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

	// OwnedFiles lists absolute paths of files known to belong to this job:
	// everything present in DownloadDir when processJob starts (populated
	// automatically — see postproc.go's processJob), plus every file later
	// produced by unpack/join/rename stages. Extension and sample cleanup
	// restrict deletion to this set when it is non-nil, mirroring upstream
	// SABnzbd's "Track files during cleanup to prevent removing unrelated
	// files" fix (commit 5b3cf86f6, #3462): cleanup must never delete a
	// file that isn't this job's own, even if DownloadDir were ever shared
	// or reused. A nil map means "not tracked" and disables the restriction
	// (used by callers/tests that construct a Job directly without going
	// through processJob).
	OwnedFiles map[string]struct{}

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

	// DirectUnpackSkipped holds sets that DirectUnpack did not attempt
	// because they aren't RAR3/RAR5 (the only formats rarengine supports).
	// These sets are handled normally by the unpack stage's external unrar
	// fallback; this is expected, not an error.
	DirectUnpackSkipped map[string]directunpack.SkippedSet
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
