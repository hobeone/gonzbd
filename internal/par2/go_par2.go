package par2

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	par2engine "github.com/hobeone/par2engine/par2"
)

// GoVerify runs a native Go-based PAR2 verification using par2engine.
// It returns a VerifyResult compatible with the existing par2 package API.
// onLine is called for progress updates and diagnostic messages (may be nil).
func GoVerify(ctx context.Context, log *slog.Logger, parfile string, onLine func(string)) (res VerifyResult, err error) {
	// Recover from panics in the par2engine library (untrusted data).
	defer func() {
		if p := recover(); p != nil {
			res.Status = StatusUnknown
			err = fmt.Errorf("go_par2: par2engine panic during verify: %v", p)
		}
	}()

	engineLog := newPar2UILogger(log.With("component", "go_par2"), onLine)

	d, err := par2engine.NewDecoder(ctx, parfile, 0, 0, engineLog)
	if err != nil {
		res.Status = StatusInvalidPar2
		return res, fmt.Errorf("go_par2: open decoder: %w", err)
	}
	defer d.Close() //nolint:errcheck // best-effort close

	if onLine != nil {
		onLine("[go_par2] Starting verification...")
	}
	progressChan := make(chan par2engine.Progress, 100)
	go func() {
		for p := range progressChan {
			if onLine != nil {
				onLine(fmt.Sprintf("[go_par2] Verifying... %.1f%%", p.Percent))
			}
		}
	}()

	if err := d.VerifyScans(ctx, progressChan); err != nil {
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
// onLine is called for each progress update and diagnostic message (may be nil).
func GoRepair(ctx context.Context, log *slog.Logger, parfile string, onLine func(string)) (res RepairResult, err error) {
	// Recover from panics in the par2engine library (untrusted data).
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("go_par2: par2engine panic during repair: %v", p)
		}
	}()

	engineLog := newPar2UILogger(log.With("component", "go_par2"), onLine)

	d, err := par2engine.NewDecoder(ctx, parfile, 0, 0, engineLog)
	if err != nil {
		return res, fmt.Errorf("go_par2: open decoder: %w", err)
	}
	defer d.Close() //nolint:errcheck // best-effort close

	res.CommandLine = fmt.Sprintf("go_par2 repair %s", parfile)

	// Phase 1: Verify
	if onLine != nil {
		onLine("[go_par2] Starting verification...")
	}
	verifyProgress := make(chan par2engine.Progress, 100)
	go func() {
		for p := range verifyProgress {
			if onLine != nil {
				onLine(fmt.Sprintf("[go_par2] Verifying... %.1f%%", p.Percent))
			}
		}
	}()

	if err := d.VerifyScans(ctx, verifyProgress); err != nil {
		return res, fmt.Errorf("go_par2: verify scan: %w", err)
	}

	counts := d.ShardCounts()

	if !counts.RepairNeeded() {
		res.Success = true
		res.Output = "All files are correct"
		if onLine != nil {
			onLine("[go_par2] All files are correct")
		}
		return res, nil
	}

	if !counts.RepairPossible() {
		res.NeedMoreBlocks = true
		res.BlocksNeeded = counts.BlocksNeeded()
		res.Output = fmt.Sprintf("Repair is not possible. You need %d more recovery blocks.", res.BlocksNeeded)
		if onLine != nil {
			onLine(fmt.Sprintf("[go_par2] Repair not possible — need %d more recovery blocks", res.BlocksNeeded))
		}
		return res, nil
	}

	// Phase 2: Repair
	if onLine != nil {
		onLine(fmt.Sprintf("[go_par2] Repair required: %d missing blocks, %d parity available",
			counts.UnusableDataShardCount, counts.UsableParityShardCount))
	}
	repairProgress := make(chan par2engine.Progress, 100)
	go func() {
		for p := range repairProgress {
			if onLine != nil {
				onLine(fmt.Sprintf("[go_par2] Repairing... %.1f%%", p.Percent))
			}
		}
	}()

	if err := d.Repair(ctx, repairProgress); err != nil {
		res.ExitCode = 1
		res.Output = fmt.Sprintf("Repair Failed: %v", err)
		if onLine != nil {
			onLine(fmt.Sprintf("[go_par2] Repair failed: %v", err))
		}
		return res, fmt.Errorf("go_par2: repair: %w", err)
	}

	res.Success = true
	res.Output = "Repair complete"
	if onLine != nil {
		onLine("[go_par2] Repair complete")
	}

	return res, nil
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

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	if h.onLine != nil {
		if line, ok := h.formatForUI(r); ok {
			h.onLine(line)
		}
	}
	return err
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{Handler: h.Handler.WithAttrs(attrs), onLine: h.onLine}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{Handler: h.Handler.WithGroup(name), onLine: h.onLine}
}

// noisyMessages contains par2engine Info-level messages that are too internal
// to show in the UI output stream.
var noisyMessages = map[string]bool{
	"Parsed SliceByteCount":                          true,
	"Configured memory-limited streaming":            true,
	"skipping packet with mismatching set ID":        true,
	"skipping volume packet with mismatching set ID": true,
}

// statusMessages are Info-level messages we always want to show.
var statusMessages = map[string]bool{
	"No repair needed. All files are healthy.": true,
	"Starting pipelined repair...":             true,
	"Repair completed successfully!":           true,
}

// formatForUI decides whether a log record should appear in the UI and formats it.
func (h *teeHandler) formatForUI(r slog.Record) (string, bool) {
	if noisyMessages[r.Message] {
		return "", false
	}

	if r.Level < slog.LevelWarn {
		// For Info: show only if the record has a file/name/err attribute,
		// or if it's an explicit status message.
		if !statusMessages[r.Message] {
			hasRelevant := false
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "file", "name", "err":
					hasRelevant = true
					return false
				}
				return true
			})
			if !hasRelevant {
				return "", false
			}
		}
	}

	var sb strings.Builder
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
			sb.WriteString(fmt.Sprintf("%v", a.Value.Any()))
		} else {
			sb.WriteString(a.Value.String())
		}
		return true
	})

	return "[go_par2] " + sb.String(), true
}
