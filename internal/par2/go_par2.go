package par2

import (
	"context"
	"fmt"
	"log/slog"

	par2engine "github.com/hobeone/par2engine/par2"
)

// GoVerify runs a native Go-based PAR2 verification using par2engine.
// It returns a VerifyResult compatible with the existing par2 package API.
func GoVerify(ctx context.Context, log *slog.Logger, parfile string) (res VerifyResult, err error) {
	// Recover from panics in the par2engine library (untrusted data).
	defer func() {
		if p := recover(); p != nil {
			res.Status = StatusUnknown
			err = fmt.Errorf("go_par2: par2engine panic during verify: %v", p)
		}
	}()

	log = log.With("component", "go_par2")

	d, err := par2engine.NewDecoder(ctx, parfile, 0, 0, log)
	if err != nil {
		res.Status = StatusInvalidPar2
		return res, fmt.Errorf("go_par2: open decoder: %w", err)
	}
	defer d.Close()

	progressChan := make(chan par2engine.Progress, 100)
	go func() {
		for range progressChan {
			// drain progress — verify is fast
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
	case counts.RepairPossible():
		res.Status = StatusRepairPossible
	default:
		res.Status = StatusRepairNotPossible
	}

	return res, nil
}

// GoRepair runs a native Go-based PAR2 verification and repair using par2engine.
// It returns a RepairResult compatible with the existing par2 package API.
// onLine is called for each progress update (may be nil).
func GoRepair(ctx context.Context, log *slog.Logger, parfile string, onLine func(string)) (res RepairResult, err error) {
	// Recover from panics in the par2engine library (untrusted data).
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("go_par2: par2engine panic during repair: %v", p)
		}
	}()

	log = log.With("component", "go_par2")

	d, err := par2engine.NewDecoder(ctx, parfile, 0, 0, log)
	if err != nil {
		return res, fmt.Errorf("go_par2: open decoder: %w", err)
	}
	defer d.Close()

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
