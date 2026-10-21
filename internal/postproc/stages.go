// Package postproc implements the post-processing orchestrator for gonzbd.
// A single worker goroutine drains one FIFO queue (ppQueue), running the
// registered Stage implementations in order for each job.
package postproc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// QuickCheckOutcome is what the quickcheck stage was able to determine about
// a job's file CRCs. It replaces the QuickCheckRan/QuickCheckPassed pair,
// which could not express its fourth combination and so collapsed two
// unrelated states into one.
//
// The distinction that matters is between "I found nothing wrong" and "I have
// nothing to say". Both used to read as !QuickCheckRan. Making the states
// exhaustive means a reader has to name which one it means, so that collapse
// cannot be rebuilt by accident. RepairStage.Run skips par2 for Clean and
// Unidentified and runs it for every other outcome.
type QuickCheckOutcome int

const (
	// QuickCheckNotRun means nothing was attempted, because the stage was
	// disabled or found no par2 sets to check against. It is no verdict, so
	// repair runs. A DirectUnpack success does not stand in for one: it
	// covers only the RAR sets DirectUnpack extracted.
	QuickCheckNotRun QuickCheckOutcome = iota

	// QuickCheckClean means every par2-tracked file's CRC32 matched. Repair can
	// skip the expensive par2 subprocess outright — verification already
	// confirmed integrity.
	QuickCheckClean

	// QuickCheckDamaged means verification ran and found unverifiable or
	// mismatched files. Repair runs.
	QuickCheckDamaged

	// QuickCheckInconclusive means verification was attempted and could not
	// complete. No claim is being made about the data either way, and repair
	// runs, as it does for QuickCheckNotRun. The two stay separate because
	// they report different things: here par2 sets may well exist and nothing
	// has checked them.
	//
	// It is also the default the stage adopts the moment it knows par2 sets
	// exist, narrowing to Clean, Damaged or Unidentified only where
	// identification or verification actually happened (#314). So the causes are "anything that did not earn a
	// verdict", and only one of them involves the manifest — do not go
	// looking for a manifest problem first:
	//   - the par2 scan itself failed, so whether the job has par2 sets is
	//     unknown (the one case set before the default applies);
	//   - par2 sets were found but par2.Assess errored before
	//     verifying any of them;
	//   - the job's manifest was unreadable or described no files, so there
	//     were no expected CRCs to compare against;
	//   - par2 sets were found and no assembled CRC was available for any of
	//     them, so the comparison had nothing on either side;
	//   - a par2 set read as nothing (par2.Identification.Unreadable) while
	//     the job holds a recovery volume (par2VolumesHeld), so the files it
	//     protects were never checked, whatever the other sets show;
	//   - every par2 set read as nothing and none is held
	//     (unverifiedPar2Sets), so there was nothing to verify and repair
	//     has nothing to run.
	QuickCheckInconclusive

	// QuickCheckUnidentified is the stage's judgement that every par2 set in
	// the job that describes any file is a Layout B set — par2 protecting
	// what an archive extracts to, rather than the archive — so that repair,
	// which runs before unpack, has no set to work on. Repair declines, unpack
	// extracts, and extracted_repair runs par2 against what it extracted.
	//
	// The judgement is made set by set, and it is a heuristic, not an
	// observation of Layout B. Its basis is the conditions in deferredSets.
	// A job where only some sets are deferred records the verdict on the
	// others instead, and repair skips the deferred ones
	// (Job.deferredPar2Sets).
	QuickCheckUnidentified
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
		QuickCheckUnidentified,
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
	case QuickCheckUnidentified:
		return "unidentified"
	default:
		return "unknown"
	}
}

// Stage is the interface every post-processing stage must implement.
// Stages are registered once at construction time and run in that order
// for every job. Stage errors are recorded but the pipeline continues; each
// stage self-gates based on job flags (ParError, UnpackError), except that
// runStage itself skips a payloadDeleter on a failed job.
type Stage interface {
	// Name returns a short, stable identifier used in log output and the
	// StageLog.  It should be lowercase with no spaces (e.g. "repair",
	// "unpack", "finalize").
	Name() string

	// Run executes the stage.  The supplied ctx is cancelled either when the
	// PostProcessor is stopped, or when this specific job is removed via
	// CancelJob while it's being processed; stages MUST respect it and return
	// promptly in both cases. Returning a non-nil error records the failure
	// in the StageLog but does NOT abort the pipeline; subsequent stages
	// still run.
	Run(ctx context.Context, job *Job) error
}

// Job is the post-processing unit of work.  It wraps the download-queue Job
// with post-proc-specific state.
// stages accumulate their results into the fields below.
type Job struct {
	// Job is the rehomed job record from internal/job.
	Job *job.Job

	// Ingest / Queue metadata needed by post-processing stages:
	Filename      string
	NZBBackup     string
	Category      string
	Password      string
	MetaPasswords []string // MetaPasswords holds all <meta type="password"> entries from the NZB header, tried in order after Password.
	Script        string
	PP            int
	Priority      int
	URL           string

	// DownloadDir is the absolute path where the assembler wrote the job's
	// files — the working directory for in-place stages (par2, unpack,
	// deobfuscate). Mirrors Python's nzo.download_path. Must be
	// set by the caller before the job is pushed to the PostProcessor.
	DownloadDir string

	// FinalDir is the absolute path where the job's files should end up
	// after all post-processing stages have finished. Usually a sub-path
	// of the complete directory named after the job name.
	FinalDir string

	// outputFiles is the job's file list exported to scripts as SAB_FILES:
	// sorted, slash-separated paths relative to DownloadDir as the script
	// stage sees it. outputFilesErr is set instead when finalize could not
	// produce one. They are assigned only inside recordOutputFiles and
	// recordOutputFilesErr (`git grep -n 'j\.outputFiles[A-Za-z]* = ' -- '*.go' ':!*_test.go'`
	// finds 6 lines, all in stage_finalize.go) and read by outputFileList;
	// neither is persisted, so a job reloaded after a restart has neither
	// (see the delivered-recovery branch in postproc.go).
	outputFiles    []string
	outputFilesSet bool
	outputFilesErr error

	// scriptLog and scriptLine are the user script's captured output and its
	// one-line summary, both empty when no script ran. recordScriptOutput is
	// their only writer (`git grep -nE 'j[.]script(Log|Line) = ' -- '*.go' ':!*_test.go'`
	// finds 2 lines, both in it); history files them as script_log and
	// script_line.
	scriptLog  string
	scriptLine string

	// Sanitize defines the naming replacement options for this job.
	Sanitize fsutil.SanitizeOptions

	// Unwanted is the job's standing against the unwanted-extension check,
	// read from its queue header at hand-over. UnwantedCleanupStage removes
	// nothing from an approved job; the finalizer files it on the history
	// entry.
	Unwanted unwanted.State

	// StageLog accumulates one entry per stage, in execution order.
	StageLog []StageLogEntry

	// FailMsg is set by a stage that considers the whole job failed, or by
	// the caller before the job is pushed. processJob skips every stage for
	// a job that arrives with it set, sets it itself when the pre-check finds
	// no usable download directory, and reads it for the summary's
	// Completed/Failed headline; it is one component of FailReason, which is
	// what history and finalize's log line report.
	FailMsg string

	// ParError is set by the repair and extracted_repair stages, UnpackError
	// by the unpack stage. unpack reads ParError; extracted_repair,
	// par2_cleanup and script read both, and Failed reads both with FailMsg.
	// runStage keeps the error a stage returned as the cause (see FailReason).
	// Scripts surface them via the pp_status argv and SAB_PP_STATUS env var.
	ParError    bool
	UnpackError bool

	// parCause and unpackCause are the error text of the stage that set
	// ParError and UnpackError. They are written in recordStageFailure and
	// read in FailReason. A flag set without a stage error has no cause.
	parCause    string
	unpackCause string

	// shortfall is the recovery blocks the one par2 set of the job lacked when
	// the stage that newly set ParError reported it so (ShortfallError), and
	// 0 when it is unknown: another cause, or more than one set. It is a
	// function of the stage error that set ParError, so recordStageFailure
	// writes it with parCause: `git grep -n 'j[.]shortfall = ' -- '*.go' ':!*_test.go'`
	// finds 2 lines, both in it. Read through RepairShortfall.
	shortfall int

	// quickCheck is what the quickcheck stage was able to determine about
	// this job's file CRCs. See QuickCheckOutcome. QuickCheckStage is its
	// only writer: `git grep -n '[.]quickCheck =' -- '*.go' ':!*_test.go'`
	// finds 5 lines, all in stage_quickcheck.go.
	quickCheck QuickCheckOutcome

	// deferredPar2Sets names the par2 sets (par2.Set.Name) that quickcheck
	// judged to protect files unpack has yet to extract (deferredSets).
	// repair skips them and extracted_repair runs them once unpack has
	// finished. QuickCheckStage.recordVerdict is its only writer:
	// `git grep -n '[.]deferredPar2Sets =' -- '*.go' ':!*_test.go'` finds 1 line.
	deferredPar2Sets []string

	// unverifiedPar2Sets names the par2 sets (par2.Set.Name) that read as
	// nothing (par2.Identification.Unreadable) while the job held no recovery
	// volume (par2VolumesHeld), so nothing fetched later can make them
	// readable. quickcheck leaves them out of its verdict and repair skips
	// them; the files they protect are delivered unverified. The judgement
	// is quickcheck's alone, so with quickcheck disabled the list is empty
	// and repair runs such a set, and fails on it, as it did before the list
	// existed. QuickCheckStage.recordVerdict is its only writer:
	// `git grep -n '[.]unverifiedPar2Sets =' -- '*.go' ':!*_test.go'` finds 1 line.
	unverifiedPar2Sets []string

	// deferredPar2Verified reports that extracted_repair verified, or
	// repaired, every set in deferredPar2Sets against the extracted files.
	// par2_cleanup keeps the par2 files of a job with deferred sets until
	// this is true. ExtractedRepairStage.Run is its only writer:
	// `git grep -n '[.]deferredPar2Verified =' -- '*.go' ':!*_test.go'` finds 1 line.
	deferredPar2Verified bool

	// OutputLines is a scratch buffer that stages populate with tool output
	// lines (e.g. par2 stdout, unrar output). runStage moves these into
	// StageLogEntry.Lines after each stage runs and clears the buffer.
	// Stages should append to this slice rather than writing to Lines
	// directly, since StageLogEntry is created by the orchestrator.
	OutputLines []string

	// ConsumedFiles lists absolute paths of files consumed by repair/join
	// operations (par2 volumes, joinable split files). Extension cleanup
	// skips these to prevent deletion of files needed for recovery.
	ConsumedFiles map[string]struct{}

	// Par2Renames maps par2's canonical filename → actual on-disk filename.
	// Populated from par2's "is a match for" output during repair. No
	// production code reads it: outside tests it appears only in this
	// declaration and recordRepairSuccess (stage_repair.go).
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
	// because they are not RAR5. The unpack stage extracts these sets
	// normally; this is expected, not an error.
	DirectUnpackSkipped map[string]directunpack.SkippedSet

	// pendingDeletions lists paths relative to DownloadDir of extracted
	// archives (from UnpackStage) and par2/backup files (from Par2CleanupStage)
	// that are kept in DownloadDir until FinalizeStage has moved the job to
	// FinalDir (#768), so a crash before finalize leaves the download directory
	// intact for a rerun. Intermediate stages (extracted_repair,
	// sample_cleanup, recover_par2_names, deobfuscate, unwanted_cleanup,
	// extension_cleanup) skip them, and FinalizeStage deletes them once the
	// final move succeeds. recordPendingDeletion, loadPendingDeletions and
	// clearPendingDeletions are its writers
	// (`git grep -n 'j[.]pendingDeletions = ' -- '*.go' ':!*_test.go'` finds
	// 2 lines, both in stages.go; loadPendingDeletions records through
	// recordPendingDeletion).
	pendingDeletions []string

	// par2Backups lists paths relative to DownloadDir of numbered ".1"/".2"
	// backup files created by par2 repair for targets that repair modified.
	// Kept separate from pendingDeletions until Par2CleanupStage checks its
	// gates (enable_par_cleanup, ParError, UnpackError), while still allowing
	// RecoverPar2NamesStage (which runs before Par2CleanupStage) to exclude
	// them via par2RenameExclusions. recordPar2Backup is its writer
	// (`git grep -n 'j[.]par2Backups = ' -- '*.go' ':!*_test.go'` finds 1 line,
	// in stages.go).
	par2Backups []string
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

	// Lines holds any log lines emitted by the stage. Stages do not see the
	// entry: they append to Job.OutputLines, which runStage moves here.
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

// Failed reports whether the job has failed: repair or unpack failed, or a
// stage or the caller set FailMsg. It is the one definition of a failed job.
// Its readers are finalize, the summary headline, runStage's payloadDeleter
// gate and the history entry (`git grep -n '[.]Failed()' -- '*.go' ':!*_test.go'`
// finds 4 lines).
func (j *Job) Failed() bool {
	return j.ParError || j.UnpackError || j.FailMsg != ""
}

// FailReason is the one statement of why a failed job failed: the cause of
// each failed stage, then FailMsg, joined by "; ". It is empty exactly when
// Failed is false. A repair or unpack failure with no recorded cause reads
// "repair failed" or "unpack failed".
func (j *Job) FailReason() string {
	var reasons []string
	if j.ParError {
		reasons = append(reasons, cmp.Or(j.parCause, "repair failed"))
	}
	if j.UnpackError {
		reasons = append(reasons, cmp.Or(j.unpackCause, "unpack failed"))
	}
	if j.FailMsg != "" {
		reasons = append(reasons, j.FailMsg)
	}
	return strings.Join(reasons, "; ")
}

// recordStageFailure keeps err as the cause of whichever of ParError and
// UnpackError the stage that returned it newly set. parBefore and
// unpackBefore are those flags as they were before the stage ran.
func (j *Job) recordStageFailure(parBefore, unpackBefore bool, err error) {
	if err == nil {
		return
	}
	if j.ParError && !parBefore {
		j.parCause = err.Error()
		j.shortfall = 0
		if sf, ok := errors.AsType[*ShortfallError](err); ok {
			j.shortfall = sf.Blocks
		}
	}
	if j.UnpackError && !unpackBefore {
		j.unpackCause = err.Error()
	}
}

// RepairShortfall reports the recovery blocks a failed repair lacked, net of
// those on disk, for a job whose one par2 set reported them. ok is false when
// the failure had another cause, the job has several sets, or the shortfall is
// not known; the caller must not read that as a shortfall of 0.
func (j *Job) RepairShortfall() (blocks int, ok bool) {
	return j.shortfall, j.shortfall > 0
}

// payloadDeleter marks a stage that removes files from DownloadDir as it runs,
// rather than recording them in pendingDeletions for finalize. runStage does
// not run such a stage for a failed job (Job.Failed): a failed job's
// directory stays as it is, for a retry to resume from (#907). The marked
// stages are sample_cleanup, unwanted_cleanup and extension_cleanup
// (`git grep -n '^func .* deletesPayload()' -- '*.go'` finds 3 lines).
type payloadDeleter interface {
	Stage
	deletesPayload()
}

// ScriptLog returns the user script's combined stdout and stderr, as
// captured up to MaxLogBytes, or "" when no script ran.
func (j *Job) ScriptLog() string { return j.scriptLog } //nocover: single-field getter

// ScriptLine returns the one-line summary of the user script's run
// (formatScriptLine), or "" when no script ran.
func (j *Job) ScriptLine() string { return j.scriptLine } //nocover: single-field getter

// HasRecord reports whether this post-processing job wraps a download job record.
func (j *Job) HasRecord() bool {
	return j.Job != nil
}

// par2VolumesHeld reports whether the job still holds a recovery volume back
// (job.Job.HasDeferredPar2). It decides whether a par2 set that reads as
// nothing can still be made readable: a volume repeats the packets an index
// lost, and FindPar2Files reads one in place of the index once it is on disk,
// so while one is held, a failed repair is what fetches it; with none held,
// nothing fetched later can. quickcheck reads it to record such a set in
// unverifiedPar2Sets. The retry half of the same fact is read by the app's
// finalizer, through HasDeferredPar2 itself (heldVolumesMightRepair in
// internal/app): a repair that fails with a volume held is retried to fetch
// it, which is what quickcheck relies on while one is held.
func (j *Job) par2VolumesHeld() bool {
	return j.Job != nil && j.Job.HasDeferredPar2()
}

// par2Unverified reports whether quickcheck recorded the named par2 set as
// one nothing can make readable (unverifiedPar2Sets).
func (j *Job) par2Unverified(set string) bool {
	return slices.Contains(j.unverifiedPar2Sets, set)
}

// par2Deferred reports whether quickcheck deferred the named par2 set until
// after unpack.
func (j *Job) par2Deferred(set string) bool {
	return slices.Contains(j.deferredPar2Sets, set)
}

// pendingDeletionRel normalizes path to a clean path relative to j.DownloadDir,
// or returns ("", false) when path is empty or outside j.DownloadDir.
func (j *Job) pendingDeletionRel(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	rel := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(rel) {
		if j.DownloadDir == "" {
			return "", false
		}
		r, err := filepath.Rel(j.DownloadDir, rel)
		if err != nil {
			return "", false
		}
		rel = r
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// recordPendingDeletion records path (relative to DownloadDir) for deletion
// after FinalizeStage moves the job's files (#768). Returns true when path is
// inside DownloadDir and was recorded.
func (j *Job) recordPendingDeletion(path string) bool {
	rel, ok := j.pendingDeletionRel(path)
	if !ok {
		return false
	}
	if !slices.Contains(j.pendingDeletions, rel) {
		j.pendingDeletions = append(j.pendingDeletions, rel)
	}
	return true
}

// loadPendingDeletions replaces the recorded list with the one finalize wrote
// to its sidecar (a JSON array of paths), recording each entry through
// recordPendingDeletion so an entry outside DownloadDir is dropped. On a
// decode error the list is left as it was.
func (j *Job) loadPendingDeletions(data []byte) error {
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return fmt.Errorf("decode pending deletions: %w", err)
	}
	j.clearPendingDeletions()
	for _, p := range paths {
		j.recordPendingDeletion(p)
	}
	return nil
}

// clearPendingDeletions forgets every recorded path, once finalize has
// deleted them.
func (j *Job) clearPendingDeletions() {
	j.pendingDeletions = nil
}

// isPendingDeletion reports whether path (relative to DownloadDir or absolute
// within DownloadDir) has been recorded for deletion at finalize (#768).
func (j *Job) isPendingDeletion(path string) bool {
	if len(j.pendingDeletions) == 0 {
		return false
	}
	rel, ok := j.pendingDeletionRel(path)
	return ok && slices.Contains(j.pendingDeletions, rel)
}

// outputFileList returns the list finalize recorded, or the error it recorded
// instead. A job finalize never ran for has neither, which is an error here
// rather than an empty list: an exported [] must mean "no files".
func (j *Job) outputFileList() ([]string, error) {
	switch {
	case j.outputFilesErr != nil:
		return nil, j.outputFilesErr
	case !j.outputFilesSet:
		return nil, errors.New("no file list was recorded for the job (finalize did not run)")
	}
	return j.outputFiles, nil
}

// pendingDeletionSet returns a set of the relative paths in pendingDeletions,
// or nil when none are recorded.
func (j *Job) pendingDeletionSet() map[string]struct{} {
	if len(j.pendingDeletions) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(j.pendingDeletions))
	for _, rel := range j.pendingDeletions {
		set[rel] = struct{}{}
	}
	return set
}

// recordPar2Backup records a par2-created ".N" backup path (relative to
// DownloadDir) so RecoverPar2NamesStage skips it and Par2CleanupStage can
// promote it to pendingDeletions when par2 cleanup is enabled.
func (j *Job) recordPar2Backup(path string) {
	rel, ok := j.pendingDeletionRel(path)
	if !ok {
		return
	}
	if !slices.Contains(j.par2Backups, rel) {
		j.par2Backups = append(j.par2Backups, rel)
	}
}

// par2RenameExclusions returns the union of pendingDeletions and par2Backups
// as a relative-path set for RecoverPar2NamesStage, or nil when both are empty.
func (j *Job) par2RenameExclusions() map[string]struct{} {
	if len(j.pendingDeletions) == 0 && len(j.par2Backups) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(j.pendingDeletions)+len(j.par2Backups))
	for _, rel := range j.pendingDeletions {
		set[rel] = struct{}{}
	}
	for _, rel := range j.par2Backups {
		set[rel] = struct{}{}
	}
	return set
}

// JobID returns the job identifier.
func (j *Job) JobID() string {
	if j.Job != nil {
		return j.Job.ID()
	}
	return ""
}

// Name returns the job display name.
func (j *Job) Name() string {
	if j.Job != nil {
		return j.Job.Name()
	}
	return ""
}

// RecordFileRename updates the underlying queue job's recorded filename when a
// top-level file in DownloadDir is renamed in place from `from` to `to`.
func (j *Job) RecordFileRename(from, to string) {
	if j == nil || j.Job == nil {
		return
	}
	j.Job.RecordFileRename(from, to)
}

// Manifest provides read-only access to the job's manifest for post-processing stages.
type Manifest interface {
	NumFiles() int
	Files() *job.FileTable
	FileRange(fi int) (int, int)
	ArticleBytes(i int) int
}

// Progress provides read-only access to the job's progress for post-processing stages.
type Progress interface {
	FileFilename(fi int) string
	FileAssembledCRC32(fi int) uint32
	FileFetchPolicy(fi int) job.FetchPolicy
	ExpectedBytes() int64
	ProgressFigures() (expected, remaining, failed int64)
	DownloadSeconds() int64
	FailedBytes() int64
	DownloadStarted() time.Time
	DownloadFinished() time.Time
	ContentFailedBytes() int64
	RecoveryReleased(m job.RecoveryClassifier) (fetched, held, total int)
	HasPar2Verdict() bool
	Par2ReleaseReason() string
	ServerStats() map[string]int64
	ArticleDone(i int) bool
	ArticleFailed(i int) bool
}

// Manifest returns the job's manifest.
//
//nolint:ireturn // interface bridge for stages
func (j *Job) Manifest() (Manifest, error) {
	if j.Job != nil {
		return j.Job.Manifest()
	}
	return nil, job.ErrNotResident
}

// Progress returns the job's progress record.
//
//nolint:ireturn // interface bridge for stages
func (j *Job) Progress() Progress {
	if j.Job != nil {
		p := j.Job.Progress()
		if p == nil {
			return nil
		}
		return p
	}
	return nil
}

// NumFiles returns the number of files in the job.
func (j *Job) NumFiles() int {
	if j.Job != nil {
		return j.Job.NumFiles()
	}
	return 0
}
