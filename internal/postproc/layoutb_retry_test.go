package postproc

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// par2StagesCleanup is par2Stages with unpack's archive cleanup set as given.
func par2StagesCleanup(cleanup bool) []Stage {
	up := NewUnpackStageWith(unpack.Options{UseGoRAR: true, UseGo7z: true}, cleanup)
	up.SetEnabled(true)
	qc := &QuickCheckStage{Log: slog.New(slog.DiscardHandler), Unpack: up}
	qc.SetEnabled(true)
	repair := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
	extracted := NewExtractedRepairStage(repair)
	extracted.Log = slog.New(slog.DiscardHandler)
	par2Cleanup := NewPar2CleanupStage(true)
	par2Cleanup.Log = slog.New(slog.DiscardHandler)
	finalize := NewFinalizeStage()
	finalize.Log = slog.New(slog.DiscardHandler)
	return []Stage{qc, repair, up, extracted, par2Cleanup, finalize}
}

// A Layout B job whose recovery volumes were held back fails its extracted
// repair. The app then retries it with those volumes released (#651), and the
// retry post-processes the same download directory, where the first run's
// extraction and archive still are (#768 defers archive deletion to a
// successful finalize, so the failed first run keeps release.rar even when
// archive cleanup is on). On the successful retry, finalize deletes the
// archive when cleanup is on and keeps it when off.
func TestLayoutB_RetryWithTheHeldVolumeRepairsTheExtraction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		cleanup     bool
		archiveKept bool
	}{
		{"archive cleanup on (default)", true, false},
		{"archive cleanup off", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first, dir := deliveredJob(t, "layout_b", []string{"feature.par2"}, []deliveredFile{damagedNoDigest(t)})
			if errs := runStages(t, first, par2StagesCleanup(tc.cleanup)...); !first.ParError {
				t.Fatalf("fixture guard: the first run did not fail its extracted repair; stage errors: %v", errs)
			}
			if _, err := os.Stat(filepath.Join(dir, "release.rar")); err != nil {
				t.Fatalf("fixture guard: archive was deleted on the failed first run: %v", err)
			}

			// The retry downloads the held volume into the same directory. Its
			// manifest lists the archive, which retained progress keeps
			// complete, so it is not downloaded again.
			vol := fixtureBytes(t, "layout_b", "feature.vol0+1.par2")
			if err := os.WriteFile(filepath.Join(dir, "feature.vol0+1.par2"), vol, 0o644); err != nil {
				t.Fatal(err)
			}
			retry, _ := deliveredJob(t, "layout_b", nil, []deliveredFile{damagedNoDigest(t)})
			if err := os.RemoveAll(retry.DownloadDir); err != nil {
				t.Fatal(err)
			}
			retry.DownloadDir = dir
			retry.FinalDir = dir

			errs := runStages(t, retry, par2StagesCleanup(tc.cleanup)...)

			if retry.ParError || retry.UnpackError {
				t.Fatalf("retry: ParError=%v UnpackError=%v (QuickCheck=%s); stage errors: %v",
					retry.ParError, retry.UnpackError, retry.QuickCheck, errs)
			}
			assertFeatureExtracted(t, dir)
			if _, err := os.Stat(filepath.Join(dir, "release.rar")); (err == nil) != tc.archiveKept {
				t.Errorf("archive present = %v after the successful retry, want %v", err == nil, tc.archiveKept)
			}
		})
	}
}
