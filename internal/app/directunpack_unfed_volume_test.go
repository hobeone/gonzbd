package app

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestHandOver_AVolumeNeverFedToTheDirectUnpack_DoesNotHoldTheJob: a change
// made mid-download can stop the rest of a job's volumes reaching the
// DirectUnpacker already extracting its set, while the download itself
// finishes. The hand-over then awaits an unpacker that will get no more
// volumes; the wait must end, with the set failed and left to
// post-processing, rather than hold the job until removal or shutdown.
func TestHandOver_AVolumeNeverFedToTheDirectUnpack_DoesNotHoldTheJob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*testing.T, *Application, *job.Job)
	}{
		{
			// maybeStart returns before Add (api mode=change_opts, or a
			// category change, reaches Dispatcher.SetPP).
			name: "post-processing lowered below unpack",
			change: func(t *testing.T, a *Application, j *job.Job) {
				if err := a.dispatcher.SetPP(j.ID(), job.ClampPP(1)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// completeFinalizedFile's gate skips maybeStart.
			name: "direct_unpack switched off",
			change: func(_ *testing.T, a *Application, _ *job.Job) {
				a.config.With(func(c *config.Config) { c.PostProc.DirectUnpack = false })
			},
		},
		{
			name: "enable_unrar switched off",
			change: func(_ *testing.T, a *Application, _ *job.Job) {
				a.config.With(func(c *config.Config) { c.PostProc.EnableUnrar = false })
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application, j := fetchingRarApp(t)
			id := j.ID()
			const changeBefore = 5 // volumes 1-5 are fed, 6-10 are not
			for i := range rarVolumes {
				if i == changeBefore {
					tc.change(t, application, j)
				}
				completeFile(t, application, id, i)
			}
			du := application.duOrch.unpackerForTest(id)
			if du == nil {
				t.Fatal("precondition: no DirectUnpacker was started")
			}
			if !j.IsComplete() {
				t.Fatal("precondition: the download is not complete")
			}

			application.maybeFinalizeJob(j, "")
			select {
			case <-du.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("the hand-over's DirectUnpack wait did not end within 5s; "+
					"the job is held before post-processing (status %+v)", du.Status())
			}
			const want = "volume 6 of set multi_new never arrived"
			if f, ok := du.Failures()["multi_new"]; !ok || f.Reason != want {
				t.Errorf("Failures()[multi_new] = %+v, %v; want reason %q", f, ok, want)
			}
		})
	}
}
