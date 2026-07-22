package postproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/cmdutil"
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

	// Version is the gonzbd version string (SAB_VERSION env-only).
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

	// BytesTried is total bytes attempted including retries (SAB_BYTES_TRIED).
	BytesTried int64

	// Password is the per-job password (SAB_PASSWORD env-only).
	Password string

	// Encrypted indicates whether the archive was encrypted (SAB_ENCRYPTED: 0=unknown, 1=yes).
	Encrypted int

	// Priority is the job priority (SAB_PRIORITY).
	Priority int

	// Repair indicates par2 repair was performed (SAB_REPAIR: 0/1).
	Repair int

	// Unpack indicates unpacking was performed (SAB_UNPACK: 0/1).
	Unpack int

	// Age is the age of the NZB in days since posting date (SAB_AGE).
	Age int

	// Par2Command is the par2 binary path (SAB_PAR2_COMMAND).
	Par2Command string

	// UnrarCommand is the unrar binary path (SAB_RAR_COMMAND).
	UnrarCommand string

	// SevenzCommand is the 7z binary path (SAB_7ZIP_COMMAND).
	SevenzCommand string

	// OnLine is called for each line of subprocess output. May be nil.
	OnLine func(string)

	// RedactSecrets when true causes SAB_API_KEY and SAB_PASSWORD to be
	// replaced with a placeholder in the script environment.
	RedactSecrets bool
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

// redactedValue is the placeholder used for sensitive env vars when
// redaction is enabled.
const redactedValue = "**REDACTED**"

// buildEnv constructs the environment for the script. It starts from the
// parent process environment (so PATH, HOME, etc. are inherited) and appends
// SAB_* pairs. This matches Python's os.environ.copy() + update pattern.
//
// When redact is true, SAB_API_KEY and SAB_PASSWORD are replaced with
// a placeholder to prevent leaking secrets to untrusted scripts.
func buildEnv(in ScriptInput, redact bool) []string {
	apiKey := in.APIKey
	password := in.Password
	if redact {
		apiKey = redactedValue
		password = redactedValue
	}

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
		fmt.Sprintf("SAB_API_KEY=%s", apiKey),
		fmt.Sprintf("SAB_API_URL=%s", in.APIURL),

		// Additional SABnzbd-compatible env vars:
		fmt.Sprintf("SAB_BYTES_TRIED=%d", in.BytesTried),
		fmt.Sprintf("SAB_PASSWORD=%s", password),
		fmt.Sprintf("SAB_ENCRYPTED=%d", in.Encrypted),
		fmt.Sprintf("SAB_PRIORITY=%d", in.Priority),
		fmt.Sprintf("SAB_REPAIR=%d", in.Repair),
		fmt.Sprintf("SAB_UNPACK=%d", in.Unpack),
		fmt.Sprintf("SAB_AGE=%d", in.Age),
		fmt.Sprintf("SAB_PAR2_COMMAND=%s", in.Par2Command),
		fmt.Sprintf("SAB_RAR_COMMAND=%s", in.UnrarCommand),
		fmt.Sprintf("SAB_7ZIP_COMMAND=%s", in.SevenzCommand),

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

// lastNonEmptyLine returns the content after the final newline in s, trimmed
// of trailing whitespace. If the final segment is whitespace-only it returns
// "". For typical script output (which ends with a meaningful line) this is
// equivalent to "last non-empty line", but it does NOT walk backwards through
// prior lines when the last segment is blank.
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
	cmd.Env = buildEnv(in, in.RedactSecrets)
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

	streamer := cmdutil.NewLineStreamerCapped(in.OnLine, MaxLogBytes)
	cmd.Stdout = streamer
	cmd.Stderr = streamer

	started := time.Now()

	// Retry on ETXTBSY ("text file busy"). On Linux, this transient error
	// occurs when exec races with a recently-written file whose fd has not
	// yet been fully released. Back off briefly and rebuild the command.
	const maxRetries = 5
	var startErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			// Exponential backoff: 1ms, 2ms, 4ms, 8ms, 16ms.
			time.Sleep(time.Millisecond << uint(attempt-1))

			// Rebuild the command because exec.Cmd is not reusable after Start.
			//nolint:gosec // G204: script path is operator-configured
			cmd = exec.CommandContext(ctx, scriptPath, argv...)
			cmd.Env = buildEnv(in, in.RedactSecrets)
			if in.FinalDir != "" {
				cmd.Dir = in.FinalDir
			}
			setProcessGroup(cmd)
			cmd.Cancel = func() error {
				killProcessGroup(cmd)
				return nil
			}
			streamer = cmdutil.NewLineStreamerCapped(in.OnLine, MaxLogBytes)
			cmd.Stdout = streamer
			cmd.Stderr = streamer
		}
		startErr = cmd.Start()
		if startErr == nil {
			break
		}
		if !isETXTBSY(startErr) {
			return ScriptResult{
				ExitCode: -1,
				Duration: time.Since(started),
				Err:      fmt.Errorf("RunScript: failed to start %q: %w", scriptPath, startErr),
			}
		}
	}
	if startErr != nil {
		return ScriptResult{
			ExitCode: -1,
			Duration: time.Since(started),
			Err:      fmt.Errorf("RunScript: failed to start %q after %d retries: %w", scriptPath, maxRetries, startErr),
		}
	}

	waitErr := cmd.Wait()
	duration := time.Since(started)
	logBody := streamer.String()

	// Scan log to ensure LogBody ends on a line boundary when truncated.
	if len(logBody) >= MaxLogBytes {
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
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
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
	return buildEnvMapRedact(in, false)
}

// buildEnvMapRedact returns a map of all SAB_* vars with optional redaction.
func buildEnvMapRedact(in ScriptInput, redact bool) map[string]string {
	pairs := buildEnv(in, redact)
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, _ := strings.Cut(pair, "=")
		m[k] = v
	}
	return m
}
