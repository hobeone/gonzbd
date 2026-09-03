package postproc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// ScriptStage executes a user-defined post-processing script after extraction.
type ScriptStage struct {
	mu sync.RWMutex
	// ScriptDir is the directory holding user scripts; the job's Script
	// field is resolved relative to it. May be absolute for portability.
	ScriptDir string

	// CompleteDir is the root complete directory surfaced to scripts as
	// SAB_COMPLETE_DIR and argv[1]. Distinct from Job.DownloadDir which
	// is the per-job incomplete working path.
	CompleteDir string

	// Version, APIKey, APIURL populate the corresponding SAB_* env vars.
	Version string
	APIKey  string
	APIURL  string

	// scriptCanFail when true causes non-zero script exit codes to be
	// logged but NOT treated as pipeline errors. This matches Python's
	// cfg.script_can_fail() behavior. Default false = non-zero exit
	// is an error. Atomic so SetScriptCanFail can be called from any goroutine.
	scriptCanFail atomic.Bool

	// redactSecrets when true causes SAB_API_KEY and SAB_PASSWORD to be
	// replaced with a placeholder in the script environment, preventing
	// secret leakage to untrusted scripts. Default false preserves
	// backward compatibility. Atomic for runtime toggleability.
	redactSecrets atomic.Bool

	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// SetScriptCanFail enables or disables treating non-zero script exit codes as
// warnings at runtime without restart. Thread-safe.
func (s *ScriptStage) SetScriptCanFail(v bool) { s.scriptCanFail.Store(v) }

// SetRedactSecrets enables or disables masking of SAB_API_KEY and SAB_PASSWORD
// in the script environment at runtime without restart. Thread-safe.
func (s *ScriptStage) SetRedactSecrets(v bool) { s.redactSecrets.Store(v) }

// SetScriptDir thread-safely updates the scripts directory path at runtime.
func (s *ScriptStage) SetScriptDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ScriptDir = dir
}

// NewScriptStage constructs a ScriptStage.
func NewScriptStage(scriptDir, completeDir, version, apiKey, apiURL string) *ScriptStage {
	return &ScriptStage{
		ScriptDir:   scriptDir,
		CompleteDir: completeDir,
		Version:     version,
		APIKey:      apiKey,
		APIURL:      apiURL,
	}
}

// Name returns the stage identifier.
func (*ScriptStage) Name() string { return "script" }

// Run builds a ScriptInput from the job and invokes RunScript. Returns nil
// when no script is configured or the script exits 0; wraps the RunScript
// error otherwise.
func (s *ScriptStage) Run(ctx context.Context, job *Job) error {
	s.mu.RLock()
	scriptDir := s.ScriptDir
	s.mu.RUnlock()

	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "script", "job", job.Job.ID())

	name := job.Meta.Script
	if name == "" || strings.EqualFold(name, "none") || strings.EqualFold(name, "default") {
		logf(ctx, log, job, slog.LevelDebug, "No script configured")
		return nil
	}
	if scriptDir == "" {
		err := fmt.Errorf("script %q configured on job but no script_dir is set", name)
		logf(ctx, log, job, slog.LevelWarn, "Error: %v", err)
		return err
	}
	if filepath.IsAbs(name) {
		err := fmt.Errorf("script path %q is absolute, which is not allowed; must be relative to script_dir", name)
		logf(ctx, log, job, slog.LevelWarn, "Error: %v", err)
		return err
	}
	scriptPath := filepath.Join(scriptDir, name)
	resolvedPath, err := fsutil.ResolveAndVerifyContainment(scriptDir, scriptPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("script %q not found in script_dir %q: %w", name, scriptDir, err)
		} else {
			err = fmt.Errorf("script path %q traverses outside script_dir %q: %w", name, scriptDir, err)
		}
		logf(ctx, log, job, slog.LevelWarn, "Error: %v", err)
		return err
	}
	scriptPath = resolvedPath

	// SABnzbd script status codes (§6.5):
	// 0 = success, 1 = repair failed, 2 = unpack failed, 3 = both failed
	status := 0
	if job.ParError {
		status |= 1 // bit 0: repair failure
	}
	if job.UnpackError {
		status |= 2 // bit 1: unpack failure
	}
	// A general failure message without specific par/unpack flag → status 1
	if status == 0 && job.FailMsg != "" {
		status = 1
	}

	logf(ctx, log, job, slog.LevelInfo, "Running: %s", scriptPath)

	in := ScriptInput{
		FinalDir:    job.DownloadDir,
		CompleteDir: s.CompleteDir,
		NZBName:     job.Meta.Filename,
		JobName:     job.Job.Name(),
		Category:    job.Meta.Category,
		Status:      status,
		PPFlags:     job.Meta.PP,
		ScriptName:  name,
		NZOID:       job.Job.ID(),
		URL:         job.Meta.URL,
		Version:     s.Version,
		APIKey:      s.APIKey,
		APIURL:      s.APIURL,
		// ExpectedBytes, not TotalBytes(): TotalBytes() is the immutable
		// whole-manifest total and still includes a discarded (FetchNever)
		// or still-deferred (FetchIfNeeded) recovery volume that will never
		// reach disk. ExpectedBytes excludes both, matching what
		// buildHistoryEntry reports for the same job (see
		// internal/app/history_helper.go) — a script reading SAB_BYTES for
		// an on-demand-par2 job that verified clean must see what was
		// actually fetched, not the pre-discard total.
		Bytes:         job.Job.Progress().ExpectedBytes(),
		RedactSecrets: s.redactSecrets.Load(),
		OnLine: func(line string) {
			if job.OnOutput != nil {
				job.OnOutput("script", line)
			}
		},
	}
	logf(ctx, log, job, slog.LevelInfo, "Script env: dir=%s, category=%s, status=%d, job=%s",
		in.FinalDir, in.Category, in.Status, in.JobName)

	res := RunScript(ctx, scriptPath, in)

	// Capture script output for the stage log.
	if res.LogBody != "" {
		job.OutputLines = append(job.OutputLines,
			toolOutputLines(res.LogBody)...)
	}
	logf(ctx, log, job, slog.LevelInfo, "Exit code: %d (%.1fs)", res.ExitCode, res.Duration.Seconds())

	if res.Err != nil {
		if errors.Is(res.Err, ErrNonZeroExit) {
			logf(ctx, log, job, slog.LevelWarn, "Error: script %q exited %d", name, res.ExitCode)
			if s.scriptCanFail.Load() {
				// Log but don't fail the pipeline.
				logf(ctx, log, job, slog.LevelInfo, "script_can_fail=true: ignoring non-zero exit")
				return nil
			}
			job.FailMsg = fmt.Sprintf("Script %s failed (exit=%d)", name, res.ExitCode)
			return fmt.Errorf("script %q exited %d", name, res.ExitCode)
		}
		logf(ctx, log, job, slog.LevelWarn, "Error: script %q failed: %v", name, res.Err)
		return fmt.Errorf("script %q: %w", name, res.Err)
	}
	return nil
}

// FinalizeStage moves the completed job from job.DownloadDir to job.FinalDir.
// If FinalDir is not set, it defaults to placing the job folder (named after
// its ID) in the system's complete directory.
//
// When FolderRename is true:
//   - On success: destination gets _UNPACK_ prefix during move, then prefix
//     is stripped after the rename completes. This prevents media managers
//     (Sonarr, Plex) from importing incomplete downloads.
//   - On failure: the download directory is renamed in-place with a _FAILED_
//     prefix (e.g. /incomplete/MyRelease → /incomplete/_FAILED_MyRelease).
//     Files stay in the incomplete area so retry can find them.
