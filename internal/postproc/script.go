package postproc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// MaxLogBytes caps the captured log to avoid unbounded memory use on a
// misbehaving script that produces a torrent of stdout. Excess bytes are
// silently dropped; LogBody always ends on a line boundary when truncated.
const MaxLogBytes = 512 * 1024 // 512 KiB

// ErrNonZeroExit is returned (wrapped) when the script exits non-zero.
var ErrNonZeroExit = errors.New("post-processing script exited non-zero")

// ScriptInput is the fully-resolved input to a user post-processing script.
// Every string is included in both the positional argv and as an SAB_* env
// var for backwards compatibility with scripts written for Python SABnzbd.
//
// Positional order (matching Python's external_processing argv):
//
//	script <complete_dir> <nzb_name> <job_name> <report_name>
//	       <category> <group> <status> <failure_url>
//
// Note: Python's external_processing does NOT use FinalDir as a positional
// arg or SAB_FINAL_PROCESSING_DIR. The argv positional args match the Python
// source exactly. SAB_FINAL_PROCESSING_DIR is included as an env-only
// extension for Go scripts that need the processed directory.
type ScriptInput struct {
	// FinalDir is the directory where the job was processed.
	// Included as env-only SAB_FINAL_PROCESSING_DIR (not a positional arg in Python).
	FinalDir string

	// CompleteDir is the root complete directory (positional 1; SAB_COMPLETE_DIR).
	CompleteDir string

	// NZBName is the original NZB filename (positional 2; SAB_FILENAME / SAB_NZB_NAME).
	// In Python this is nzo.filename → SAB_FILENAME. We set both for compat.
	NZBName string

	// JobName is the post-deobfuscation job name (positional 3; SAB_FINAL_NAME).
	JobName string

	// ReportName is the upstream report name (positional 4; SAB_REPORTNAME).
	// In Python this is always an empty string positional. Kept for compat.
	ReportName string

	// Category is the NZB category (positional 5; SAB_CAT).
	Category string

	// Group is the Usenet group (positional 6; SAB_GROUP).
	// In Python external_processing positional 6 is nzo.group.
	Group string

	// Status is an integer status code (positional 7; SAB_PP_STATUS).
	// In Python external_processing positional 7 is the int status.
	Status int

	// FailureURL is the failure URL if any (positional 8; SAB_FAILURE_URL).
	FailureURL string

	// PPFlags is the postproc level mask (SAB_PP env-only — from ENV_NZO_FIELDS).
	PPFlags int

	// ScriptName is the script name (SAB_SCRIPT env-only — from ENV_NZO_FIELDS).
	ScriptName string

	// NZOID is the internal job ID (SAB_NZO_ID env-only — from ENV_NZO_FIELDS).
	NZOID string

	// DownloadTime is the download duration in seconds (SAB_DOWNLOAD_TIME env-only).
	DownloadTime int

	// URL is the source URL if download came from URL (SAB_URL env-only — from ENV_NZO_FIELDS).
	URL string

	// Version is the sabnzbd-go version string (SAB_VERSION env-only).
	Version string

	// APIKey is the API key (SAB_API_KEY env-only).
	APIKey string

	// APIURL is the API endpoint URL (SAB_API_URL env-only).
	APIURL string

	// Bytes is the total bytes of the job (SAB_BYTES env-only).
	Bytes int64

	// BytesDownloaded is bytes actually downloaded (SAB_BYTES_DOWNLOADED env-only).
	BytesDownloaded int64

	// AvgBPS is average bytes/sec during download (SAB_AVG_BPS env-only).
	AvgBPS int64
}

// ScriptResult captures the outcome of a script invocation.
type ScriptResult struct {
	// ExitCode is the script's exit status, or -1 if the process could not be
	// started or was killed by a signal.
	ExitCode int

	// LogBody is combined stdout+stderr, up to MaxLogBytes.
	LogBody string

	// LastLine is the last non-empty line of LogBody (used in history UI).
	LastLine string

	// Duration is how long the script ran.
	Duration time.Duration

	// Err is non-nil on exec failure, ctx cancellation, or non-zero exit.
	Err error
}

// cappedWriter is an io.Writer that discards bytes written beyond cap bytes.
// It tracks whether the cap was hit so callers can note truncation.
type cappedWriter struct {
	buf     *bytes.Buffer
	cap     int
	written int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.cap - w.written
	if remaining <= 0 {
		return n, nil // pretend we wrote it, but discard
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	written, err := w.buf.Write(p)
	w.written += written
	return n, err
}

// buildEnv constructs the environment for the script. It starts from the
// parent process environment (so PATH, HOME, etc. are inherited) and appends
// SAB_* pairs. This matches Python's os.environ.copy() + update pattern.
func buildEnv(in ScriptInput) []string {
	base := os.Environ()
	sab := []string{
		// From ENV_NZO_FIELDS (Python's field loop):
		fmt.Sprintf("SAB_BYTES=%d", in.Bytes),
		fmt.Sprintf("SAB_BYTES_DOWNLOADED=%d", in.BytesDownloaded),
		fmt.Sprintf("SAB_CAT=%s", in.Category),
		"SAB_FAIL_MSG=",
		fmt.Sprintf("SAB_FILENAME=%s", in.NZBName),   // Python: nzo.filename → SAB_FILENAME
		fmt.Sprintf("SAB_FINAL_NAME=%s", in.JobName), // Python: nzo.final_name → SAB_FINAL_NAME
		fmt.Sprintf("SAB_GROUP=%s", in.Group),
		fmt.Sprintf("SAB_NZO_ID=%s", in.NZOID),
		fmt.Sprintf("SAB_PP=%d", in.PPFlags),
		fmt.Sprintf("SAB_SCRIPT=%s", in.ScriptName),
		fmt.Sprintf("SAB_STATUS=%d", in.Status),
		fmt.Sprintf("SAB_URL=%s", in.URL),

		// From extra_env_fields in Python's external_processing:
		fmt.Sprintf("SAB_FAILURE_URL=%s", in.FailureURL),
		fmt.Sprintf("SAB_COMPLETE_DIR=%s", in.CompleteDir),
		fmt.Sprintf("SAB_PP_STATUS=%d", in.Status),
		fmt.Sprintf("SAB_DOWNLOAD_TIME=%d", in.DownloadTime),
		fmt.Sprintf("SAB_AVG_BPS=%d", in.AvgBPS),

		// From create_env's always-present extra_env_fields:
		fmt.Sprintf("SAB_VERSION=%s", in.Version),
		fmt.Sprintf("SAB_API_KEY=%s", in.APIKey),
		fmt.Sprintf("SAB_API_URL=%s", in.APIURL),

		// Go-extension env vars (not in Python):
		fmt.Sprintf("SAB_FINAL_PROCESSING_DIR=%s", in.FinalDir),
		fmt.Sprintf("SAB_NZB_NAME=%s", in.NZBName), // alias for SAB_FILENAME for clarity
		fmt.Sprintf("SAB_REPORTNAME=%s", in.ReportName),
	}
	return append(base, sab...)
}

// buildArgv constructs the positional argv for the script, matching Python's
// external_processing command list exactly:
//
//	[script, complete_dir, nzo.filename, nzo.final_name, "", nzo.cat, nzo.group, status, failure_url]
func buildArgv(in ScriptInput) []string {
	return []string{
		in.CompleteDir,
		in.NZBName,
		in.JobName,
		in.ReportName, // Python always passes "" here; we pass ReportName for extension
		in.Category,
		in.Group,
		fmt.Sprintf("%d", in.Status),
		in.FailureURL,
	}
}

// lastNonEmptyLine walks the log body backwards and returns the last
// non-empty trimmed line.
func lastNonEmptyLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	idx := strings.LastIndexAny(s, "\n")
	if idx < 0 {
		return strings.TrimRight(s, "\r\n ")
	}
	return strings.TrimRight(s[idx+1:], "\r\n ")
}

// RunScript invokes the user script at scriptPath with positional args
// and SAB_* env vars derived from in. The script's cwd is set to
// in.FinalDir if non-empty, otherwise the caller's working directory.
//
// ctx cancellation kills the script (SIGKILL via exec.CommandContext).
// The log is captured in full up to MaxLogBytes.
//
// A non-zero exit status produces a non-nil Err wrapping ErrNonZeroExit;
// errors.Is(err, ErrNonZeroExit) recovers it. This lets callers distinguish
// script-reported failures from Go-side exec failures.
//
// Scripts killed by a signal report ExitCode -1 on Unix (or 128+signum
// depending on the OS and Go runtime version); any non-zero exit is wrapped
// as ErrNonZeroExit.
func RunScript(ctx context.Context, scriptPath string, in ScriptInput) ScriptResult {
	if scriptPath == "" {
		return ScriptResult{
			ExitCode: -1,
			Err:      fmt.Errorf("RunScript: scriptPath is empty"),
		}
	}

	if err := ctx.Err(); err != nil {
		return ScriptResult{
			ExitCode: -1,
			Err:      err,
		}
	}

	argv := buildArgv(in)
	//nolint:gosec // G204: script path is operator-configured (cfg.script_dir + entry)
	cmd := exec.CommandContext(ctx, scriptPath, argv...)
	cmd.Env = buildEnv(in)
	if in.FinalDir != "" {
		cmd.Dir = in.FinalDir
	}

	// Put the script in its own process group so that when the context is
	// cancelled and we kill the group, grandchildren (e.g. subshell "sleep 30")
	// are also killed and the stdout pipe closes promptly.
	setProcessGroup(cmd)

	// Override the default Cancel to kill the entire process group rather
	// than just the direct process. This ensures cmd.Wait() returns promptly
	// even when the script has spawned grandchildren.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}

	cw := &cappedWriter{buf: &bytes.Buffer{}, cap: MaxLogBytes}
	cmd.Stdout = cw
	cmd.Stderr = cw

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return ScriptResult{
			ExitCode: -1,
			Duration: time.Since(started),
			Err:      fmt.Errorf("RunScript: failed to start %q: %w", scriptPath, err),
		}
	}

	waitErr := cmd.Wait()
	duration := time.Since(started)
	logBody := cw.buf.String()

	// Scan log to ensure LogBody ends on a line boundary when truncated.
	if cw.written >= cw.cap {
		// Walk backwards to find the last newline so we don't emit a partial line.
		if idx := strings.LastIndexByte(logBody, '\n'); idx >= 0 {
			logBody = logBody[:idx+1]
		}
	}

	result := ScriptResult{
		ExitCode: 0,
		LogBody:  logBody,
		LastLine: lastNonEmptyLine(logBody),
		Duration: duration,
	}

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
		result.Err = fmt.Errorf("%w (exit code %d): %w", ErrNonZeroExit, result.ExitCode, waitErr)
	}

	return result
}

// buildEnvMap returns a map of all SAB_* vars for testing/inspection.
// Not exported; used by tests.
func buildEnvMap(in ScriptInput) map[string]string {
	pairs := buildEnv(in)
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, _ := strings.Cut(pair, "=")
		m[k] = v
	}
	return m
}

// Ensure bufio is used (needed by an alternative scanning approach kept for
// future reference). Remove if unused after final review.
var _ = bufio.NewScanner
