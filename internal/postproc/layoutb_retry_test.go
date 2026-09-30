package postproc

import (
	"os"
	"path/filepath"
	"testing"
)

// A Layout B job whose recovery volumes were held back fails its extracted
// repair. The app then retries it with those volumes released (#651), and the
// retry post-processes the same download directory: the first run's
// extraction is still in it, and so is the archive, which the failed run did
// not delete. The second run, with the recovery volume on disk, must repair
// the extracted file rather than trip over what the first run left.
func TestLayoutB_RetryWithTheHeldVolumeRepairsTheExtraction(t *testing.T) {
	t.Parallel()

	first, dir := deliveredJob(t, "layout_b", []string{"feature.par2"}, []deliveredFile{damagedB2(t)})
	if errs := runStages(t, first, par2Stages()...); !first.ParError {
		t.Fatalf("fixture guard: the first run did not fail its extracted repair; stage errors: %v", errs)
	}

	// The retry downloads the held volume into the same directory. Its job's
	// manifest lists the archive, which retained progress keeps complete, and
	// what it delivers now.
	vol := fixtureBytes(t, "layout_b", "feature.vol0+1.par2")
	if err := os.WriteFile(filepath.Join(dir, "feature.vol0+1.par2"), vol, 0o644); err != nil {
		t.Fatal(err)
	}
	retry, _ := deliveredJob(t, "layout_b", nil, []deliveredFile{damagedB2(t)})
	retry.DownloadDir = dir

	errs := runStages(t, retry, par2Stages()...)

	if retry.ParError || retry.UnpackError {
		t.Fatalf("retry: ParError=%v UnpackError=%v (QuickCheck=%s); stage errors: %v",
			retry.ParError, retry.UnpackError, retry.QuickCheck, errs)
	}
	assertFeatureExtracted(t, dir)
}
