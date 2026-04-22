package app_test

import (
	"testing"
)

// TestCheckpoint_SurvivesCrashMidDownload verifies that a crash mid-job
// loses at most one checkpoint interval of per-article/per-file progress
// rather than the entire in-memory state.
//
// The basic checkpoint machinery (dirty flag + ticker) is tested by
// TestCheckpointFires_AfterMutation and TestCheckpointSkips_WhenClean in
// checkpoint_test.go.  This scenario-level test exercises the full
// crash-recovery path (no Shutdown call, reload from disk) and requires
// the A.3 test orchestrator + an injectable fake crash to be meaningful.
//
// TODO: flesh out the crash-recovery scenario once A.3's orchestrator lands.
func TestCheckpoint_SurvivesCrashMidDownload(t *testing.T) {
	t.Skip("TODO: implement full crash-recovery scenario after A.3 orchestrator lands (B.4 checkpoint machinery is tested in checkpoint_test.go)")
}
