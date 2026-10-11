package app

import (
	"context"
	"testing"
	"time"
)

// TestResume_ReevaluatesStalls pins issue #792 and its sibling: both queue-wide
// resumes, the user's and the low-disk auto-resume, re-evaluate jobs a storage
// fault parked during the pause. The stall loop's interval is far longer than
// the test, so only a resume's own request can explain a resumed job.
func TestResume_ReevaluatesStalls(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		resume func(t *testing.T, application *Application, dir string)
	}{
		{name: "low_disk_auto_resume", resume: func(t *testing.T, a *Application, dir string) {
			t.Helper()
			if !a.tryAutoResumeLowDisk(t.Context(), dir) {
				t.Fatal("tryAutoResumeLowDisk = false with free space above the threshold, want true")
			}
		}},
		{name: "user_resume", resume: func(_ *testing.T, a *Application, _ string) { a.ResumeDownloads() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			application, _, _ := newLifecycleTestApp(t,
				WithDownloader(newFakeDownloader()),
				WithDiskProbe(newFakeDiskProbe(1<<40)),
				WithLowDiskRecheckInterval(time.Hour),
				WithStallRecheckInterval(time.Hour),
			)
			application.ctx = t.Context()
			t.Cleanup(application.stopLowDiskWatch)
			dir := t.TempDir()

			application.handleLowDisk(dir, 0)
			j := addStallTestJob(t, application, "lowdisk-stall")
			application.Stall(j.ID(), testFault("write"))
			if got := application.StallReason(j.ID()).Reason; got == "" {
				t.Fatal("the fixture is not stalled")
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go application.runStallRecheck(ctx)

			tc.resume(t, application, dir)

			waitForResumed(t, application, j.ID(),
				"a queue-wide resume did not re-evaluate a job a storage fault parked "+
					"during the pause; it stays stalled until the interval or a restart (#792)")
		})
	}
}
