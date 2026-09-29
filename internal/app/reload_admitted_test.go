package app

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestReloadDownloader_LeavesAnAdmittedJobsProgressAlone: a downloader reload
// un-fails the failed articles of every file that is not Complete, so that the
// new server set can try them. A job handed to post-processing is not
// downloaded again, so for it the reset buys nothing, and it rewrites the
// failed-byte figures its post-processing run and history entry read.
//
// The job that is not admitted is the control: its failed article is reset by
// the same reload, so the admitted job keeping its failure is the skip, not a
// reload that cleared nothing.
func TestReloadDownloader_LeavesAnAdmittedJobsProgressAlone(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })
	// Paused, so no job is granted a lease and nothing is dispatched: the
	// articles' flags are the ones this test sets.
	application.Dispatcher().Pause()

	addFailed := func(name string) *job.Job {
		t.Helper()
		parsed := &nzb.NZB{Files: []nzb.File{{
			Subject: name + ".bin",
			Bytes:   200,
			Articles: []nzb.Article{
				{ID: name + "-first@t", Bytes: 100, Number: 1},
				{ID: name + "-second@t", Bytes: 100, Number: 2},
			},
		}}}
		j, hdr, err := BuildIngestJob(application.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
		if err != nil {
			t.Fatalf("BuildIngestJob(%s): %v", name, err)
		}
		if err := application.Dispatcher().Add(t.Context(), j, hdr); err != nil {
			t.Fatalf("Add(%s): %v", name, err)
		}
		ackFailed(t, application.Dispatcher(), j.ID(), name+"-second@t")
		return j
	}
	admittedJob := addFailed("admitted")
	control := addFailed("control")

	if got := application.postProcAdmissions.admit(admittedJob, "Failed: handed off from Fetching"); got != admitted {
		t.Fatalf("admit = %v, want admitted", got)
	}
	t.Cleanup(func() { application.postProcAdmissions.release(admittedJob) })

	if err := application.ReloadDownloader(nil); err != nil {
		t.Fatalf("ReloadDownloader: %v", err)
	}

	if got := control.Progress().FailedBytes(); got != 0 {
		t.Fatalf("control job FailedBytes = %d after the reload, want 0: the reload did not "+
			"reset a failed article, so the check below would pass for no reason", got)
	}
	p := admittedJob.Progress()
	if got := p.FailedBytes(); got != 100 {
		t.Errorf("admitted job FailedBytes = %d after the reload, want 100: the reload "+
			"rewrote the progress of a job in post-processing", got)
	}
	m := mustManifest(t, admittedJob)
	for i := range m.NumArticles() {
		if m.ArticleID(i) == "admitted-second@t" && !p.ArticleFailed(i) {
			t.Errorf("admitted job's failed article %d is no longer failed after the reload", i)
		}
	}
}
