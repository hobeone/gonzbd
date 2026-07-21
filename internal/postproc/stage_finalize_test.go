package postproc

import (
	"log/slog"
	"testing"
)

func TestFinalizeHelpers(t *testing.T) {
	t.Parallel()

	// Direct reference for alignment check
	_ = (*FinalizeStage).handleFailure

	t.Run("handleFailure", func(t *testing.T) {
		f := NewFinalizeStage()
		job := &Job{
			ParError: true,
		}
		err := f.handleFailure(t.Context(), slog.Default(), job, false)
		if err != nil {
			t.Fatalf("handleFailure: %v", err)
		}
	})
}
