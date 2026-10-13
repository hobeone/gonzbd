package postproc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// Run hands SetMinFreeSpace's value to the extraction, which refuses an
// archive that would leave less than that free (#901).
func TestUnpackStage_MinFreeSpaceReachesExtraction(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		minFree     int64
		wantRefused bool
	}{
		{"no filesystem has this free", int64(1) << 62, true},
		{"zero", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job, dir := stageJob(t)
			buildTestTar(t, dir, "archive.tar")

			// BaseOpts' own value is replaced by the stage's.
			s := NewUnpackStageWith(unpack.Options{MinFreeSpace: int64(1) << 62}, false)
			s.SetEnabled(true)
			s.EnableTar = true
			s.SetMinFreeSpace(tc.minFree)

			err := s.Run(t.Context(), job)
			if refused := errors.Is(err, unpack.ErrInsufficientSpace); refused != tc.wantRefused {
				t.Fatalf("Run = %v, want refused %v", err, tc.wantRefused)
			}
			if !tc.wantRefused && err != nil {
				t.Fatalf("Run: %v", err)
			}
			if job.UnpackError != tc.wantRefused {
				t.Errorf("UnpackError = %v, want %v", job.UnpackError, tc.wantRefused)
			}
			_, statErr := os.Stat(filepath.Join(dir, "payload.txt"))
			if extracted := statErr == nil; extracted == tc.wantRefused {
				t.Errorf("payload.txt extracted = %v with min free space %d", extracted, tc.minFree)
			}
		})
	}
}
