package postproc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestFailedJob_DeletingStagesKeepItsFiles pins that no stage deletes a file
// from a failed job's download directory, however the job came to fail: the
// directory is what a retry resumes from (#907). The control case proves each
// stage deletes what it is configured to when the job has not failed, so the
// failed cases are not passing because nothing was ever going to be removed.
func TestFailedJob_DeletingStagesKeepItsFiles(t *testing.T) {
	files := []string{"movie.part01.rar", "movie-sample.mkv", "movie.nfo", "movie.par2", "Setup.exe"}
	deletable := []string{"movie-sample.mkv", "movie.nfo", "movie.par2", "Setup.exe"}

	cases := []struct {
		name   string
		fail   func(*Job)
		failed bool
	}{
		{"not failed", func(*Job) {}, false},
		{"repair failed", func(j *Job) { j.ParError = true }, true},
		{"unpack failed", func(j *Job) { j.UnpackError = true }, true},
		{"a stage set a fail message", func(j *Job) { j.FailMsg = "a stage failed the job" }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r, err := unwanted.NewRules(unwanted.ActionFail, unwanted.ModeBlacklist, []string{"exe"})
			if err != nil {
				t.Fatal(err)
			}
			sample := NewSampleCleanupStage()
			sample.SetEnabled(true)
			// The failure is set by a stage, as repair, unpack or a cleanup
			// stage would set it, so the pre-check does not skip the run.
			p := New(Options{Stages: []Stage{
				&mockPPStage{name: "fail", runFunc: func(_ context.Context, j *Job) error { c.fail(j); return nil }},
				sample,
				NewUnwantedCleanupStage(func() (unwanted.Rules, error) { return r, nil }),
				NewExtensionCleanupStage([]string{"nfo", "par2"}),
			}})
			job := &Job{Job: newQueueJob(t, "test", 3), PP: 3, DownloadDir: dir}

			p.processJob(t.Context(), job)

			for _, f := range deletable {
				if got := exists(t, filepath.Join(dir, f)); got != c.failed {
					t.Errorf("%s present = %v, want %v", f, got, c.failed)
				}
			}
			if !exists(t, filepath.Join(dir, "movie.part01.rar")) {
				t.Error("the archive was removed")
			}
		})
	}
}
