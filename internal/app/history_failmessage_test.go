package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// failingStage stands in for a repair or unpack stage that records its
// failure the way the real ones do: it sets the stage's error flag and
// returns the error. It is registered under the real stage's name so
// the pipeline treats it as that stage.
type failingStage struct {
	name string
	set  func(*postproc.Job)
	err  error
}

func (s failingStage) Name() string { return s.name }

func (s failingStage) Run(_ context.Context, j *postproc.Job) error {
	s.set(j)
	return s.err
}

// TestBuildHistoryEntry_FailMessageNamesTheStageError pins #984: a job that
// fails in repair or unpack is filed Failed with the stage's own error as
// its fail_message, not an empty one. The job goes through a real
// PostProcessor, which is what hands buildHistoryEntry the job.
func TestBuildHistoryEntry_FailMessageNamesTheStageError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		stage failingStage
		want  string
	}{
		{
			name: "repair",
			stage: failingStage{
				name: "repair",
				set:  func(j *postproc.Job) { j.ParError = true },
				err:  errors.New(`repair "x": need 1 more recovery blocks`),
			},
			want: `repair "x": need 1 more recovery blocks`,
		},
		{
			name: "unpack",
			stage: failingStage{
				name: "unpack",
				set:  func(j *postproc.Job) { j.UnpackError = true },
				err:  errors.New(`unpack "a.rar": CRC failed`),
			},
			want: `unpack "a.rar": CRC failed`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, qjob := buildHistoryTestJob(t, "hist-failmsg-"+c.name, "MyRelease", time.Now(), 1)
			done := make(chan *postproc.Job, 1)
			pp := postproc.New(postproc.Options{
				Stages:    []postproc.Stage{c.stage},
				OnJobDone: func(j *postproc.Job) { done <- j },
				Logger:    slog.New(slog.DiscardHandler),
			})
			if err := pp.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pp.Stop() })
			dir := t.TempDir() // the pre-check fails a job whose directory is empty
			if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			pp.Process(&postproc.Job{Job: qjob, DownloadDir: dir, PP: types.PPDelete})

			var got *postproc.Job
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("post-processing did not finish")
			}
			entry := buildHistoryEntry(got)
			if entry.Status != "Failed" {
				t.Fatalf("entry.Status = %q, want Failed", entry.Status)
			}
			if entry.FailMessage != c.want {
				t.Errorf("entry.FailMessage = %q, want %q", entry.FailMessage, c.want)
			}
		})
	}
}
