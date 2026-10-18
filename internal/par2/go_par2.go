package par2

import (
	"context"
	"errors"
	"fmt"
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

// newDecoder handles setup and candidate file registration for a PAR2
// decoder.
func newDecoder(ctx context.Context, log *slog.Logger, parfile string, candidates []string, onWarn, onInfo, onUILine func(string)) (*par2engine.Decoder, error) {
	d, err := par2engine.NewDecoder(ctx, parfile, par2engine.DecoderOptions{
		MaxFileSize: par2FileSizeLimit,
		Logger:      newPar2UILogger(log.With("component", "go_par2"), onUILine),
	})
	if err != nil {
		return nil, fmt.Errorf("open decoder: %w", err)
	}

	n, addErr := addCandidateFiles(d, filepath.Dir(parfile), candidates)
	if addErr != nil && onWarn != nil {
		onWarn(fmt.Sprintf("could not register all candidate files: %v", addErr))
	}
	if n > 0 && onInfo != nil {
		onInfo(fmt.Sprintf("registered candidate files count=%d", n))
	}

	return d, nil
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
// candidates are the files beside parfile that the engine may content-match
// against the set regardless of their names: the list RepairWith takes as
// extra files (see addCandidateFiles). nil registers none. par2engine lists
// the directory once, in loadVolumeFiles, and keeps only .par2 names from it,
// so a file in neither this list nor the set is not scanned.
// onLine is called for progress updates and diagnostic messages (may be nil).
func GoVerify(ctx context.Context, log *slog.Logger, parfile string, candidates []string, onLine func(string)) (res VerifyResult, err error) {
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

		d, decErr := newDecoder(ctx, log, parfile, candidates,
			func(msg string) { log.Warn("go_par2: "+msg, "parfile", parfile) },
			func(msg string) { log.Info("go_par2: "+msg, "parfile", parfile) },
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
		res.BlocksNeeded = counts.BlocksNeeded()
		res.DataBlocks = counts.UsableDataShardCount + counts.UnusableDataShardCount
		res.CommandLine = fmt.Sprintf("go_par2 verify %s", parfile)
		res.Stdout = fmt.Sprintf("usable_data=%d unusable_data=%d usable_parity=%d files_to_rebuild=%d files_to_rename=%d",
			counts.UsableDataShardCount, counts.UnusableDataShardCount, counts.UsableParityShardCount,
			counts.FilesNeedingRebuild, counts.RenamesNeeded)

		switch {
		case !counts.RepairNeeded():
			if problem := protectedFilesIntact(parfile); problem != "" {
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
				safeOnLine("[go_par2] Repair needed: " + repairWork(counts))
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
// candidates are as for GoVerify.
// onLine is called for each progress update and diagnostic message (may be nil).
// Success is set only when the engine reports the set correct or repaired and
// protectedFilesIntact then finds a regular file at every protected name.
func GoRepair(ctx context.Context, log *slog.Logger, parfile string, candidates []string, onLine func(string)) (res RepairResult, err error) {
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

		d, decErr := newDecoder(ctx, log, parfile, candidates,
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
			if problem := protectedFilesIntact(parfile); problem != "" {
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
		accumulate("[go_par2] Repair required: " + repairWork(counts))

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

// repairWork describes the work a repair the engine has called possible
// will do. A rebuild-only set has no missing blocks, so the block count alone
// would read as nothing to do.
func repairWork(counts par2engine.ShardCounts) string {
	return fmt.Sprintf("%d missing blocks, %d damaged file(s) to rebuild, %d misnamed file(s) to rename, %d parity available",
		counts.UnusableDataShardCount, counts.FilesNeedingRebuild, counts.RenamesNeeded, counts.UsableParityShardCount)
}

// protectedFilesIntact reports whether every file the par2 index at parfile
// protects is a regular file at its recorded name. It returns "" when that
// holds and otherwise a description of what does not, for the caller to log
// and to refuse success on. GoVerify and GoRepair each report a set clean at
// one site, behind this check:
// `git grep -n -e 'Status = StatusAllFilesOK$' -e 'Success = true$' -- internal/par2/go_par2.go`
// returns 2 lines, one in each.
//
// Whether the files' content is correct is par2engine's verdict, and gonzbd
// does not hash them again: ShardCounts.RepairNeeded() is true while any
// protected file is missing, the wrong size or fails its full-file MD5, and
// Repair() installs a rebuilt file only once its MD5 matches.
//
// This check covers what the engine does not judge. The engine opens a
// protected name through os.Root, which follows a symlink whose target
// resolves inside the directory, so such a link reads to the engine as the
// file. Lstat does not follow it, and a job holds no links.
//
// A check that cannot run — the index does not parse, the directory does not
// open — is also a refusal: the caller cannot report a set clean that it
// could not confirm, and a refusal sends the stage to the external par2
// fallback rather than to a false success.
func protectedFilesIntact(parfile string) string {
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
		// fd.FileName is poster-controlled, so confinement is os.Root rather
		// than a lexical join — see relocateFile's doc comment in fsops.go for
		// the argument. Lstat, not Stat, for the same reason relocateFile
		// checks the source with Lstat: a symlink planted at the expected path
		// should not read as "the file exists" on its target's say-so.
		info, err := root.Lstat(filepath.FromSlash(fd.FileName))
		if err != nil || !info.Mode().IsRegular() {
			bad = append(bad, fd.FileName+" (missing)")
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("%d protected file(s) are not intact: %s", len(bad), strings.Join(bad, ", "))
}

// CandidateFiles returns the absolute paths of the non-directory, non-par2
// files at the top of dir: the list a verify or repair hands the engine as
// candidates, so that every caller scans the same files.
func CandidateFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, e := range entries {
		if e.IsDir() || strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
			continue
		}
		result = append(result, filepath.Join(dir, e.Name()))
	}
	return result, nil
}

// addCandidateFiles registers each of candidates with the decoder via
// AddCandidateFile and returns how many it registered. parDir (the par2
// file's directory) and each candidate are paths as the caller would open
// them, absolute or relative to the working directory; both are made absolute
// before the candidate is taken relative to parDir, because par2engine
// resolves it under parDir and refuses one that leaves it. The caller chooses
// the list; this reads no directory. A candidate that cannot be registered is
// skipped, and the returned error names each one.
func addCandidateFiles(d *par2engine.Decoder, parDir string, candidates []string) (int, error) {
	if len(candidates) == 0 {
		return 0, nil
	}
	absDir, err := filepath.Abs(parDir)
	if err != nil {
		return 0, fmt.Errorf("resolve %s: %w", parDir, err)
	}
	var n int
	var errs []error
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c, err))
			continue
		}
		rel, err := filepath.Rel(absDir, abs)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c, err))
			continue
		}
		if err := d.AddCandidateFile(rel); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
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
