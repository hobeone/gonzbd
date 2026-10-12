package par2

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 is mandated by the PAR2 format, not security-sensitive
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	par2engine "github.com/hobeone/par2engine/par2"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// par2FileSizeLimit is the maximum size of a par2 index or volume file that
// the Go engine will process. The library default (100 MB) is too small for
// large releases; 1 GB covers all realistic NZB download sizes.
const par2FileSizeLimit = 1 * 1024 * 1024 * 1024 // 1 GiB

// newDecoderForDir handles setup and candidate file registration for a PAR2
// decoder. The returned engineVerified fills in as the decoder's verification
// report is logged; protectedFilesIntact reads it.
func newDecoderForDir(ctx context.Context, log *slog.Logger, parfile, candidateDir string, onWarn, onInfo, onUILine func(string)) (*par2engine.Decoder, *engineVerified, error) {
	verified := &engineVerified{names: make(map[string]struct{})}
	engineLog := slog.New(&verdictRecorder{
		Handler:  newPar2UILogger(log.With("component", "go_par2"), onUILine).Handler(),
		verified: verified,
	})

	d, err := par2engine.NewDecoder(ctx, parfile, par2engine.DecoderOptions{
		MaxFileSize: par2FileSizeLimit,
		Logger:      engineLog,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open decoder: %w", err)
	}

	if candidateDir != "" {
		if n, addErr := addCandidateFiles(d, candidateDir); addErr != nil {
			if onWarn != nil {
				onWarn(fmt.Sprintf("could not register all candidate files: %v", addErr))
			}
		} else if n > 0 {
			if onInfo != nil {
				onInfo(fmt.Sprintf("registered candidate files count=%d", n))
			}
		}
	}

	return d, verified, nil
}

// monitorProgress spawns a background goroutine to format and track progress
// updates from a channel, calling onUpdate with formatted strings. It returns
// the progress channel and a stop func that closes the channel and waits for
// the goroutine to exit. stop is idempotent (safe to call more than once, e.g.
// once via defer and once explicitly) so callers can defer it to guarantee
// the monitor goroutine terminates even if the engine call panics.
func monitorProgress(label string, onUpdate func(string)) (progress chan par2engine.Progress, stop func()) {
	progress = make(chan par2engine.Progress, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			onUpdate(fmt.Sprintf("[go_par2] %s... %.1f%%", label, p.Percent))
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(progress)
			<-done
		})
	}
	return progress, stop
}

// runWithProgress runs engineCall with a monitored progress channel and
// guarantees the monitor goroutine is stopped even if engineCall panics — the
// defer covers the panic-unwind path that SafeEngineRun's recover takes,
// which an inline close() after the call does not (see issue #100).
func runWithProgress(label string, onUpdate func(string), engineCall func(chan par2engine.Progress) error) error {
	progress, stop := monitorProgress(label, onUpdate)
	defer stop()
	err := engineCall(progress)
	stop()
	return err
}

// GoVerify runs a native Go-based PAR2 verification using par2engine.
// It returns a VerifyResult compatible with the existing par2 package API.
// candidateDir is the NZB download directory; every non-par2 file in it is
// registered via AddCandidateFile so the engine can content-match files
// regardless of their on-disk names. Pass an empty string to skip.
// onLine is called for progress updates and diagnostic messages (may be nil).
func GoVerify(ctx context.Context, log *slog.Logger, parfile, candidateDir string, onLine func(string)) (res VerifyResult, err error) {
	err = cmdutil.SafeEngineRun("go_par2: par2engine panic during verify", func() error {
		var mu sync.Mutex
		var safeOnLine func(string)
		if onLine != nil {
			safeOnLine = func(line string) {
				mu.Lock()
				defer mu.Unlock()
				onLine(line)
			}
		}

		d, verified, decErr := newDecoderForDir(ctx, log, parfile, candidateDir,
			func(msg string) { log.Warn("go_par2: "+msg, "dir", candidateDir) },
			func(msg string) { log.Info("go_par2: "+msg, "dir", candidateDir) },
			safeOnLine,
		)
		if decErr != nil {
			res.Status = StatusInvalidPar2
			return fmt.Errorf("go_par2: %w", decErr)
		}
		defer d.Close() //nolint:errcheck // best-effort close

		var scanErr error
		if safeOnLine != nil {
			scanErr = runWithProgress("Verifying", safeOnLine, func(progress chan par2engine.Progress) error {
				return d.VerifyScans(ctx, progress)
			})
		} else {
			scanErr = d.VerifyScans(ctx, nil)
		}
		if scanErr != nil {
			res.Status = StatusUnknown
			return fmt.Errorf("go_par2: verify scan: %w", scanErr)
		}

		counts := d.ShardCounts()
		res.CommandLine = fmt.Sprintf("go_par2 verify %s", parfile)
		res.Stdout = fmt.Sprintf("usable_data=%d unusable_data=%d usable_parity=%d",
			counts.UsableDataShardCount, counts.UnusableDataShardCount, counts.UsableParityShardCount)

		switch {
		case !counts.RepairNeeded():
			if problem := protectedFilesIntact(parfile, verified); problem != "" {
				// The engine sees nothing to repair, so whether repair is
				// possible is not something it can answer here.
				res.Status = StatusRepairRequired
				if safeOnLine != nil {
					safeOnLine(fmt.Sprintf("[go_par2] Engine reported %q, but %s", "All files are correct", problem))
				}
				break
			}
			res.Status = StatusAllFilesOK
			if safeOnLine != nil {
				safeOnLine("[go_par2] All files are correct")
			}
		case counts.RepairPossible():
			res.Status = StatusRepairPossible
			if safeOnLine != nil {
				safeOnLine(fmt.Sprintf("[go_par2] Repair needed: %d blocks missing, %d parity available",
					counts.UnusableDataShardCount, counts.UsableParityShardCount))
			}
		default:
			res.Status = StatusRepairNotPossible
			if safeOnLine != nil {
				safeOnLine(fmt.Sprintf("[go_par2] Repair not possible: need %d more recovery blocks",
					counts.BlocksNeeded()))
			}
		}

		return nil
	}, func(_ any) {
		res.Status = StatusUnknown
	})
	return res, err
}

// GoRepair runs a native Go-based PAR2 verification and repair using par2engine.
// It returns a RepairResult compatible with the existing par2 package API.
// candidateDir is the NZB download directory; every non-par2 file in it is
// registered via AddCandidateFile so the engine can content-match files
// regardless of their on-disk names. Pass an empty string to skip.
// onLine is called for each progress update and diagnostic message (may be nil).
// Success is set only when protectedFilesIntact confirms the set after the run.
func GoRepair(ctx context.Context, log *slog.Logger, parfile, candidateDir string, onLine func(string)) (res RepairResult, err error) {
	err = cmdutil.SafeEngineRun("go_par2: par2engine panic during repair", func() error {
		var mu sync.Mutex
		var output strings.Builder
		accumulate := func(line string) {
			mu.Lock()
			defer mu.Unlock()
			output.WriteString(line)
			output.WriteString("\n")
			if onLine != nil {
				onLine(line)
			}
		}

		d, verified, decErr := newDecoderForDir(ctx, log, parfile, candidateDir,
			func(msg string) { accumulate(fmt.Sprintf("[go_par2] WARN: %s", msg)) },
			func(msg string) { accumulate(fmt.Sprintf("[go_par2] %s", msg)) },
			accumulate,
		)
		if decErr != nil {
			res.Output = output.String()
			return fmt.Errorf("go_par2: %w", decErr)
		}
		defer d.Close() //nolint:errcheck // best-effort close

		// succeed is the exit for every run the engine calls a success: the
		// verdict is the engine's AND protectedFilesIntact's.
		succeed := func(engineVerdict string) {
			if problem := protectedFilesIntact(parfile, verified); problem != "" {
				accumulate(fmt.Sprintf("[go_par2] Engine reported %q, but %s", engineVerdict, problem))
			} else {
				res.Success = true
				accumulate("[go_par2] " + engineVerdict)
			}
			res.Output = output.String()
		}

		res.CommandLine = fmt.Sprintf("go_par2 repair %s", parfile)

		scanErr := runWithProgress("Verifying", accumulate, func(progress chan par2engine.Progress) error {
			return d.VerifyScans(ctx, progress)
		})
		if scanErr != nil {
			res.Output = output.String()
			return fmt.Errorf("go_par2: verify scan: %w", scanErr)
		}

		counts := d.ShardCounts()

		if !counts.RepairNeeded() {
			succeed("All files are correct")
			return nil
		}

		if !counts.RepairPossible() {
			res.NeedMoreBlocks = true
			res.BlocksNeeded = counts.BlocksNeeded()
			accumulate(fmt.Sprintf("[go_par2] Repair not possible — need %d more recovery blocks", res.BlocksNeeded))
			res.Output = output.String()
			return nil
		}

		// Phase 2: Repair
		accumulate(fmt.Sprintf("[go_par2] Repair required: %d missing blocks, %d parity available",
			counts.UnusableDataShardCount, counts.UsableParityShardCount))

		repairErr := runWithProgress("Repairing", accumulate, func(progress chan par2engine.Progress) error {
			return d.Repair(ctx, progress)
		})

		if repairErr != nil {
			res.ExitCode = 1
			accumulate(fmt.Sprintf("[go_par2] Repair failed: %v", repairErr))
			res.Output = output.String()
			return fmt.Errorf("go_par2: repair: %w", repairErr)
		}

		succeed("Repair complete")
		return nil
	})

	return res, err
}

// protectedFilesIntact is the native engine's verdict on whether a par2 set is
// intact on disk after a run: every file the index at parfile protects is a
// regular file at its recorded name whose full-file MD5 matches the index. It
// returns "" when that holds and otherwise a description of what does not,
// for the caller to log and to refuse success on. GoVerify and GoRepair each
// report a set clean at one site, behind this check:
// `git grep -n -e 'Status = StatusAllFilesOK$' -e 'Success = true$' -- internal/par2/go_par2.go`
// returns 2 lines, one in each.
//
// par2engine v1.0.7's own success signal, ShardCounts.RepairNeeded(), is not
// that verdict. It counts a block as usable wherever the scan located it, in
// any file, and ignores the per-file hash result, so it reports nothing to
// repair when a damaged file's correct blocks exist elsewhere: an intact copy
// under another name, a stored archive member, or the file's own blocks
// shifted by inserted bytes. Repair() returns early on the same signal.
//
// A file par2engine's verification report logged as OK (engineVerified) has
// already had its full-file MD5 matched by the engine with every block at its
// own offset, and Repair() writes only to a file that is flagged or has a
// block elsewhere (buildNeedsWrite in par2engine's repair.go), so hashing it
// again would only repeat a read. Every other protected file is hashed here.
// That trust fails closed: if the engine's report changes shape, nothing is
// recorded as verified and every file is hashed.
//
// A check that cannot run — the index does not parse, the directory does not
// open, a file cannot be read — is also a refusal: the caller cannot report a
// set clean that it could not confirm, and a refusal sends the stage to the
// external par2 fallback rather than to a false success.
func protectedFilesIntact(parfile string, verified *engineVerified) string {
	descs, err := ParseFileDescriptions(parfile)
	if err != nil {
		return fmt.Sprintf("the par2 index could not be re-read to confirm it: %v", err)
	}

	dir := filepath.Dir(parfile)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Sprintf("%s could not be opened to confirm it: %v", dir, err)
	}
	defer root.Close() //nolint:errcheck // read-only

	var bad []string
	for _, fd := range descs {
		if problem := protectedFileProblem(root, fd, verified); problem != "" {
			bad = append(bad, fd.FileName+" ("+problem+")")
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("%d protected file(s) are not intact: %s", len(bad), strings.Join(bad, ", "))
}

// protectedFileProblem returns "" when fd's file is intact on disk under
// root, and otherwise why it is not.
func protectedFileProblem(root *os.Root, fd FileDesc, verified *engineVerified) string {
	// fd.FileName is poster-controlled, so confinement is os.Root rather than
	// a lexical join — see relocateFile's doc comment in fsops.go for the
	// argument. Lstat, not Stat, for the same reason relocateFile checks the
	// source with Lstat: a symlink planted at the expected path should not
	// read as "the file exists" on its target's say-so.
	name := filepath.FromSlash(fd.FileName)
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return "missing"
	}
	if verified.has(fd.FileName) {
		return ""
	}
	f, err := root.Open(name)
	if err != nil {
		return fmt.Sprintf("unreadable: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	h := md5.New()  //nolint:gosec // par2 mandates MD5, not security-sensitive
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Sprintf("unreadable: %v", err)
	}
	if [16]byte(h.Sum(nil)) != fd.FullHash {
		return "hash mismatch"
	}
	return ""
}

// engineFileOKMessage is the message par2engine v1.0.7's verification report
// (Decoder.logVerificationReport) logs, with a "file" attribute naming the
// par2 entry, for a protected file whose MD5 it matched.
const engineFileOKMessage = "File status: OK"

// engineVerified is the set of par2 entry names par2engine's verification
// report logged as OK. verdictRecorder writes it; protectedFilesIntact reads
// it.
type engineVerified struct {
	mu    sync.Mutex
	names map[string]struct{}
}

func (v *engineVerified) add(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.names[name] = struct{}{}
}

// has reports whether name was logged as OK. A nil set holds nothing.
func (v *engineVerified) has(name string) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.names[name]
	return ok
}

// verdictRecorder is an slog.Handler on par2engine's logger that records each
// engineFileOKMessage into verified, whether or not the wrapped handler is
// enabled at that level, and otherwise passes records through unchanged.
type verdictRecorder struct {
	slog.Handler
	verified *engineVerified
}

func (h *verdictRecorder) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo || h.Handler.Enabled(ctx, level)
}

func (h *verdictRecorder) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == engineFileOKMessage {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "file" {
				h.verified.add(a.Value.String())
				return false
			}
			return true
		})
	}
	if !h.Handler.Enabled(ctx, r.Level) {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h *verdictRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &verdictRecorder{Handler: h.Handler.WithAttrs(attrs), verified: h.verified}
}

func (h *verdictRecorder) WithGroup(name string) slog.Handler {
	return &verdictRecorder{Handler: h.Handler.WithGroup(name), verified: h.verified}
}

// addCandidateFiles reads dir and registers every non-directory, non-par2 file
// with the decoder via AddCandidateFile. The paths are relative to the par2
// file's directory (which is the same as the NZB download directory). Files
// that fail registration are logged as warnings; the count of successfully
// registered files is returned. A non-nil error means ReadDir failed.
func addCandidateFiles(d *par2engine.Decoder, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read candidate dir: %w", err)
	}
	var n int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip par2 files — the decoder already knows about them.
		if strings.EqualFold(filepath.Ext(name), ".par2") {
			continue
		}
		if addErr := d.AddCandidateFile(name); addErr != nil {
			// Non-fatal: log and continue so a single bad path doesn't abort.
			continue
		}
		n++
	}
	return n, nil
}

// newPar2UILogger wraps base with a teeHandler so that Error and Warn records
// from par2engine — and selected Info records about files and repair status —
// are forwarded to onLine in addition to the normal log output.
// If onLine is nil, base is returned unchanged.
func newPar2UILogger(base *slog.Logger, onLine func(string)) *slog.Logger {
	if onLine == nil {
		return base
	}
	return slog.New(&teeHandler{
		Handler: base.Handler(),
		onLine:  onLine,
	})
}

// teeHandler is an slog.Handler that passes records to an underlying handler
// and also forwards selected records to the UI via onLine.
type teeHandler struct {
	slog.Handler
	onLine func(string)
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// Always enable Info level and above because the UI wants to see Info, Warn, and Error.
	if level >= slog.LevelInfo {
		return true
	}
	if h.Handler != nil {
		return h.Handler.Enabled(ctx, level)
	}
	return false
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.onLine != nil {
		if line, ok := h.formatForUI(r); ok {
			h.onLine(line)
		}
	}
	if h.Handler != nil {
		return h.Handler.Handle(ctx, r)
	}
	return nil
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{Handler: h.Handler.WithAttrs(attrs), onLine: h.onLine}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{Handler: h.Handler.WithGroup(name), onLine: h.onLine}
}

// formatForUI decides whether a log record should appear in the UI and formats it.
func (h *teeHandler) formatForUI(r slog.Record) (string, bool) {
	if r.Level < slog.LevelInfo {
		return "", false
	}

	var sb strings.Builder
	// No [go_par2] prefix here — the repair stage already identifies the tool
	// via job.OnOutput("go_par2", ...). Adding a prefix here would cause
	// double-prefixing in the live view and would displace any leading
	// whitespace that par2engine uses for indentation (e.g. the file list
	// under "PAR2 set protects N file(s):"), preventing correct styling in
	// the history view.
	switch {
	case r.Level >= slog.LevelError:
		sb.WriteString("ERROR: ")
	case r.Level >= slog.LevelWarn:
		sb.WriteString("WARN: ")
	}
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			return true // skip internal plumbing attr
		}
		sb.WriteString(" ")
		sb.WriteString(a.Key)
		sb.WriteString("=")
		if a.Value.Kind() == slog.KindAny {
			fmt.Fprintf(&sb, "%v", a.Value.Any())
		} else {
			sb.WriteString(a.Value.String())
		}
		return true
	})

	return sb.String(), true
}
