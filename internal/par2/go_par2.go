package par2

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	par2engine "github.com/hobeone/par2engine/par2"
)

// par2FileSizeLimit is the maximum size of a par2 index or volume file that
// the Go engine will process. The library default (100 MB) is too small for
// large releases; 1 GB covers all realistic NZB download sizes.
const par2FileSizeLimit = 1 * 1024 * 1024 * 1024 // 1 GiB

// GoVerify runs a native Go-based PAR2 verification using par2engine.
// It returns a VerifyResult compatible with the existing par2 package API.
// candidateDir is the NZB download directory; every non-par2 file in it is
// registered via AddCandidateFile so the engine can content-match files
// regardless of their on-disk names. Pass an empty string to skip.
// onLine is called for progress updates and diagnostic messages (may be nil).
func GoVerify(ctx context.Context, log *slog.Logger, parfile, candidateDir string, onLine func(string)) (res VerifyResult, err error) {
	// Recover from panics in the par2engine library (untrusted data).
	defer func() {
		if p := recover(); p != nil {
			res.Status = StatusUnknown
			err = fmt.Errorf("go_par2: par2engine panic during verify: %v", p)
		}
	}()

	engineLog := newPar2UILogger(log.With("component", "go_par2"), onLine)

	d, err := par2engine.NewDecoder(ctx, parfile, par2engine.DecoderOptions{
		MaxFileSize: par2FileSizeLimit,
		Logger:      engineLog,
	})
	if err != nil {
		res.Status = StatusInvalidPar2
		return res, fmt.Errorf("go_par2: open decoder: %w", err)
	}
	defer d.Close() //nolint:errcheck // best-effort close

	if candidateDir != "" {
		if n, addErr := addCandidateFiles(d, candidateDir); addErr != nil {
			log.Warn("go_par2: could not register all candidate files", "dir", candidateDir, "err", addErr)
		} else if n > 0 {
			log.Info("go_par2: registered candidate files", "count", n, "dir", candidateDir)
		}
	}

	var verifyProgress chan par2engine.Progress
	var verifyDone chan struct{}
	if onLine != nil {
		verifyProgress = make(chan par2engine.Progress, 100)
		verifyDone = make(chan struct{})
		go func() {
			for p := range verifyProgress {
				onLine(fmt.Sprintf("[go_par2] Verifying... %.1f%%", p.Percent))
			}
			close(verifyDone)
		}()
	}

	err = d.VerifyScans(ctx, verifyProgress)
	if verifyProgress != nil {
		close(verifyProgress)
		<-verifyDone
	}
	if err != nil {
		res.Status = StatusUnknown
		return res, fmt.Errorf("go_par2: verify scan: %w", err)
	}

	counts := d.ShardCounts()
	res.CommandLine = fmt.Sprintf("go_par2 verify %s", parfile)
	res.Stdout = fmt.Sprintf("usable_data=%d unusable_data=%d usable_parity=%d",
		counts.UsableDataShardCount, counts.UnusableDataShardCount, counts.UsableParityShardCount)

	switch {
	case !counts.RepairNeeded():
		res.Status = StatusAllFilesOK
		if onLine != nil {
			onLine("[go_par2] All files are correct")
		}
	case counts.RepairPossible():
		res.Status = StatusRepairPossible
		if onLine != nil {
			onLine(fmt.Sprintf("[go_par2] Repair needed: %d blocks missing, %d parity available",
				counts.UnusableDataShardCount, counts.UsableParityShardCount))
		}
	default:
		res.Status = StatusRepairNotPossible
		if onLine != nil {
			onLine(fmt.Sprintf("[go_par2] Repair not possible: need %d more recovery blocks",
				counts.BlocksNeeded()))
		}
	}

	return res, nil
}

// GoRepair runs a native Go-based PAR2 verification and repair using par2engine.
// It returns a RepairResult compatible with the existing par2 package API.
// candidateDir is the NZB download directory; every non-par2 file in it is
// registered via AddCandidateFile so the engine can content-match files
// regardless of their on-disk names. Pass an empty string to skip.
// onLine is called for each progress update and diagnostic message (may be nil).
func GoRepair(ctx context.Context, log *slog.Logger, parfile, candidateDir string, onLine func(string)) (res RepairResult, err error) {
	// Recover from panics in the par2engine library (untrusted data).
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("go_par2: par2engine panic during repair: %v", p)
		}
	}()

	var output strings.Builder
	accumulate := func(line string) {
		output.WriteString(line)
		output.WriteString("\n")
		if onLine != nil {
			onLine(line)
		}
	}

	engineLog := newPar2UILogger(log.With("component", "go_par2"), accumulate)

	d, err := par2engine.NewDecoder(ctx, parfile, par2engine.DecoderOptions{
		MaxFileSize: par2FileSizeLimit,
		Logger:      engineLog,
	})
	if err != nil {
		res.Output = output.String()
		return res, fmt.Errorf("go_par2: open decoder: %w", err)
	}
	defer d.Close() //nolint:errcheck // best-effort close

	if candidateDir != "" {
		if n, addErr := addCandidateFiles(d, candidateDir); addErr != nil {
			accumulate(fmt.Sprintf("[go_par2] WARN: could not register all candidate files: %v", addErr))
		} else if n > 0 {
			accumulate(fmt.Sprintf("[go_par2] Registered %d candidate file(s) for content matching", n))
		}
	}

	res.CommandLine = fmt.Sprintf("go_par2 repair %s", parfile)

	var verifyProgress chan par2engine.Progress
	var verifyDone chan struct{}
	verifyProgress = make(chan par2engine.Progress, 100)
	verifyDone = make(chan struct{})
	go func() {
		for p := range verifyProgress {
			accumulate(fmt.Sprintf("[go_par2] Verifying... %.1f%%", p.Percent))
		}
		close(verifyDone)
	}()

	err = d.VerifyScans(ctx, verifyProgress)
	close(verifyProgress)
	<-verifyDone
	if err != nil {
		res.Output = output.String()
		return res, fmt.Errorf("go_par2: verify scan: %w", err)
	}

	counts := d.ShardCounts()

	if !counts.RepairNeeded() {
		res.Success = true
		accumulate("[go_par2] All files are correct")
		res.Output = output.String()
		return res, nil
	}

	if !counts.RepairPossible() {
		res.NeedMoreBlocks = true
		res.BlocksNeeded = counts.BlocksNeeded()
		accumulate(fmt.Sprintf("[go_par2] Repair not possible — need %d more recovery blocks", res.BlocksNeeded))
		res.Output = output.String()
		return res, nil
	}

	// Phase 2: Repair
	accumulate(fmt.Sprintf("[go_par2] Repair required: %d missing blocks, %d parity available",
		counts.UnusableDataShardCount, counts.UsableParityShardCount))
	repairProgress := make(chan par2engine.Progress, 100)
	repairDone := make(chan struct{})
	go func() {
		for p := range repairProgress {
			accumulate(fmt.Sprintf("[go_par2] Repairing... %.1f%%", p.Percent))
		}
		close(repairDone)
	}()

	repairErr := d.Repair(ctx, repairProgress)
	close(repairProgress)
	<-repairDone

	if repairErr != nil {
		res.ExitCode = 1
		accumulate(fmt.Sprintf("[go_par2] Repair failed: %v", repairErr))
		res.Output = output.String()
		return res, fmt.Errorf("go_par2: repair: %w", repairErr)
	}

	res.Success = true
	accumulate("[go_par2] Repair complete")
	res.Output = output.String()

	return res, nil
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
