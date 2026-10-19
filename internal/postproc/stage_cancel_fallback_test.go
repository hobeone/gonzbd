package postproc

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/testutil"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// TestDispatchRepairTool_CancelledContextSkipsFallback pins that a cancelled
// context is a decision not to continue, not an inconclusive native result: the
// external par2 retry must not start. The nonexistent main file makes the
// native run fail (err != nil), the case that retries under a live context
// (TestDispatchRepairTool_FallbackRunsWhenBinaryPresent).
func TestDispatchRepairTool_CancelledContextSkipsFallback(t *testing.T) {
	t.Parallel()

	stub := testutil.WriteExecutable(t, filepath.Join(t.TempDir(), "par2stub"), "#!/bin/sh\nexit 1\n")
	var retried bool
	job := &Job{
		Job:         newQueueJob(t, "testjob", 0),
		DownloadDir: t.TempDir(),
		OnOutput: func(_ string, line string) {
			if strings.Contains(line, "retrying with external") {
				retried = true
			}
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := dispatchRepairTool(ctx, slog.Default(), job,
		"nonexistent.par2", nil, par2.RunOptions{Command: stub}, true, true)
	if retried {
		t.Error("the external par2 retry ran on a cancelled context")
	}
	if err == nil {
		t.Error("expected the native error to be returned, got nil")
	}
}

// TestExtractWithDriver_CancelledContextSkipsFallback pins that a native
// failure under a cancelled context is returned as is: the external retry
// would fail at once on the same context.
func TestExtractWithDriver_CancelledContextSkipsFallback(t *testing.T) {
	t.Parallel()

	job, _ := stageJob(t)
	ctx, cancel := context.WithCancel(t.Context())
	var runExtCalled bool
	driver := archiveEngineDriver{
		toolName:   "mocktool",
		goToolName: "go_mocktool",
		formatName: "MOCK",
		useGo:      true,
		fallback:   true,
		findBin:    func(_ unpack.Options) (string, error) { return "/usr/local/bin/mocktool", nil },
		runGo: func(ctx context.Context, _ *slog.Logger, _ unpack.Archive, _ string, _ unpack.Options) (unpack.Result, error) {
			cancel()
			return unpack.Result{}, ctx.Err()
		},
		runExternal: func(_ context.Context, _ *slog.Logger, _ unpack.Archive, _ string, _ unpack.Options) (unpack.Result, error) {
			runExtCalled = true
			return unpack.Result{}, nil
		},
	}
	_, err := extractWithDriver(ctx, slog.Default(), job, unpack.Archive{Name: "test.mock"}, unpack.Options{}, driver)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if runExtCalled {
		t.Error("the external retry ran on a cancelled context")
	}
}
