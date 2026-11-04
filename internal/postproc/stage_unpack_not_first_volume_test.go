package postproc

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// A middle volume named like a first one is not an archive the unpack stage
// extracted. two_members.part2 holds only the continuation of second.txt; as
// obf.part1.rar it is the whole "obf" archive. With cleanup on, it must fail
// the stage and not be queued for deletion at finalize.
func TestUnpackStage_MiddleVolumeNamedAsFirstIsNotCleanedUp(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	main := filepath.Join(dir, "obf.part1.rar")
	copyRARFixture(t, "two_members.part2.rar", main)

	s := appliedUnpackStage(UnpackConfig{Base: unpack.Options{UseGoRAR: true}, EnableUnrar: true, Enable7zip: true, Cleanup: true})
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err == nil {
		t.Errorf("Run = nil, want the middle volume to fail the stage (output %q)", job.OutputLines)
	}
	if !job.UnpackError {
		t.Error("UnpackError = false, want true")
	}
	if job.isPendingDeletion(main) {
		t.Errorf("%s was queued for deletion: %v", main, job.pendingDeletions)
	}
}

// The external retry is not run for an archive missing its first volume: unrar
// is missing the same volumes, could only extract the members that start
// later, and would overwrite the missing-volume reason with its own. The
// driver is the one extractRARArchive runs, with only the external tool
// replaced.
func TestExtractWithDriver_NotFirstVolumeSkipsFallback(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	main := filepath.Join(dir, "obf.part1.rar")
	copyRARFixture(t, "two_members.part2.rar", main)
	var runExtCalled bool
	driver := rarEngineDriver(unpack.Options{UseGoRAR: true, GoRarFallback: true})
	driver.findBin = func(_ unpack.Options) (string, error) { return "/usr/local/bin/unrar", nil }
	driver.runExternal = func(_ context.Context, _ *slog.Logger, _ unpack.Archive, _ string, _ unpack.Options) (unpack.Result, error) {
		runExtCalled = true
		return unpack.Result{Reason: unpack.FailUnknown}, errors.New("unrar: no files to extract")
	}
	archive := unpack.Archive{Type: unpack.RarArchive, Name: "obf", MainFile: main, Parts: []string{main}}

	res, err := extractWithDriver(t.Context(), slog.Default(), job, archive, unpack.Options{}, driver)

	if runExtCalled {
		t.Error("the external retry ran for an archive missing its first volume")
	}
	if !unpack.IsNotFirstVolume(err) {
		t.Errorf("err = %v, want the go_unrar not-first-volume refusal", err)
	}
	if res.Reason != unpack.FailMissingVolume {
		t.Errorf("Reason = %q, want %q", res.Reason, unpack.FailMissingVolume)
	}
}
