package postproc

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/par2"
)

// fixtureShortfall is the blocks of the par2 fixture's data.bin at its 4-byte
// slices: with no recovery volume and no data file, the set lacks them all.
const fixtureShortfall = (4065 + 3) / 4

// runRepairOnce runs the repair stage the way runStage does: the stage, then
// recordStageFailure with the flags as they stood before it.
func runRepairOnce(t *testing.T, job *Job) error {
	t.Helper()
	stage := NewRepairStage()
	stage.Apply(RepairConfig{UseGoPar2: true})
	stage.Log = slog.New(slog.DiscardHandler)
	parBefore, unpackBefore := job.ParError, job.UnpackError
	err := stage.Run(t.Context(), job)
	job.recordStageFailure(parBefore, unpackBefore, err)
	return err
}

func TestRepairShortfall(t *testing.T) {
	t.Parallel()

	t.Run("one set that lacks blocks records them", func(t *testing.T) {
		t.Parallel()
		job, dir := stageJob(t)
		copyToDir(t, "../../test/fixtures/par2/data.par2", dir)
		err := runRepairOnce(t, job)
		if _, ok := errors.AsType[*ShortfallError](err); !ok {
			t.Fatalf("Run error = %v, want a *ShortfallError", err)
		}
		if blocks, ok := job.RepairShortfall(); !ok || blocks != fixtureShortfall {
			t.Errorf("RepairShortfall = %d, %v; want %d, true", blocks, ok, fixtureShortfall)
		}
	})

	t.Run("two sets record nothing", func(t *testing.T) {
		t.Parallel()
		job, dir := stageJob(t)
		idx := copyToDir(t, "../../test/fixtures/par2/data.par2", dir)
		b, err := os.ReadFile(idx)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "twin.par2"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		err = runRepairOnce(t, job)
		if err == nil || !job.ParError {
			t.Fatalf("Run error = %v, ParError = %v; want both sets to fail", err, job.ParError)
		}
		if _, ok := errors.AsType[*ShortfallError](err); ok {
			t.Errorf("Run error %v is a *ShortfallError: it names one set's shortfall for a job with two", err)
		}
		if blocks, ok := job.RepairShortfall(); ok {
			t.Errorf("RepairShortfall = %d, true; want unknown", blocks)
		}
	})

	t.Run("a stage error of another kind records nothing", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		job.ParError = true
		job.recordStageFailure(false, false, errors.New("repair \"x\": containment check: boom"))
		if blocks, ok := job.RepairShortfall(); ok {
			t.Errorf("RepairShortfall = %d, true; want unknown", blocks)
		}
	})

	t.Run("a shortfall of no blocks is unknown", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		job.ParError = true
		job.recordStageFailure(false, false, &ShortfallError{Blocks: 0})
		if blocks, ok := job.RepairShortfall(); ok {
			t.Errorf("RepairShortfall = %d, true; want unknown", blocks)
		}
	})

	t.Run("a failure that was already recorded is not overwritten", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		job.ParError = true
		job.recordStageFailure(true, false, &ShortfallError{Blocks: 9})
		if blocks, ok := job.RepairShortfall(); ok {
			t.Errorf("RepairShortfall = %d, true; want unknown: this stage did not set ParError", blocks)
		}
	})

	t.Run("a wrapped shortfall is found", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		stage := NewRepairStage()
		set := par2.Set{Name: "s", MainFile: "s.par2"}
		res := par2.RepairResult{NeedMoreBlocks: true, BlocksNeeded: 7}
		err := stage.handleRepairResult(t.Context(), slog.New(slog.DiscardHandler), job, set, nil, res, nil)
		job.recordStageFailure(false, false, err)
		if blocks, ok := job.RepairShortfall(); !ok || blocks != 7 {
			t.Errorf("RepairShortfall = %d, %v; want 7, true", blocks, ok)
		}
	})

	t.Run("an engine error beside a shortfall result records nothing", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		stage := NewRepairStage()
		set := par2.Set{Name: "s", MainFile: "s.par2"}
		res := par2.RepairResult{NeedMoreBlocks: true, BlocksNeeded: 7}
		err := stage.handleRepairResult(t.Context(), slog.New(slog.DiscardHandler), job, set, nil, res, errors.New("engine crashed"))
		job.recordStageFailure(false, false, err)
		if blocks, ok := job.RepairShortfall(); ok {
			t.Errorf("RepairShortfall = %d, true; want unknown: the run returned an error", blocks)
		}
	})

	t.Run("a later failure clears an earlier shortfall", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		job.ParError = true
		job.recordStageFailure(false, false, &ShortfallError{Blocks: 5})
		job.recordStageFailure(false, false, errors.New("other"))
		if blocks, ok := job.RepairShortfall(); ok {
			t.Errorf("RepairShortfall = %d, true; want unknown", blocks)
		}
	})
}
