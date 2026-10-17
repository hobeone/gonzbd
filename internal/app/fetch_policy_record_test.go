package app

import (
	"bytes"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// addHeldRecoveryJob queues a job of one two-article payload file and one
// recovery volume the on-demand par2 classification holds back, at PP 3, and
// returns it with the volume's file index.
func addHeldRecoveryJob(t *testing.T, application *Application, id string) (*job.Job, int) {
	t.Helper()
	parsed, err := nzb.Parse(bytes.NewReader(evictNZBWithRecoveryVolume(2, 1)))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, id+".nzb", types.FetchOptions{JobID: id, PP: 3}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, []byte("raw nzb bytes"), false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	idx := recoveryFileIndexIn(t, m)
	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Fatalf("precondition: FileFetchPolicy(%d) = %v after AddJob, want FetchIfNeeded", idx, got)
	}
	assertStoredFetchPolicy(t, application, id, idx, job.FetchIfNeeded)
	return j, idx
}

// TestPermanentArticleFailure_LeavesTheVolumeHeldUntilReleased pins, through
// both callers that record a permanent article failure, that the failure
// changes no fetch policy and the record keeps FetchIfNeeded; and that the
// volume the verdict then releases (releaseRecoveryVolumes) has FetchAlways in
// job_files after the recorder's next flush. A row left at FetchIfNeeded is
// what a restart restores (#858).
func TestPermanentArticleFailure_LeavesTheVolumeHeldUntilReleased(t *testing.T) {
	cases := []struct {
		name string
		fail func(t *testing.T, application *Application, j *job.Job)
	}{
		{"assembler rejection", func(_ *testing.T, application *Application, j *job.Job) {
			application.handleArticleRejected(j.ID(), 0, 0, "negative offset", assembler.WholeArticle)
		}},
		{"download failure", func(t *testing.T, application *Application, j *job.Job) {
			application.pipeline.handleFailureResult(t.Context(), &downloader.ArticleResult{
				Job: j, FileIdx: 0, ArtIdx: 0, MessageID: "p1@t",
				Subject: "payload.bin", Err: downloader.ErrNoServersLeft,
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			application, _, _ := newEvictionTestApp(t)
			j, idx := addHeldRecoveryJob(t, application, "releasedvolume1")

			tc.fail(t, application, j)
			if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
				t.Fatalf("FileFetchPolicy(%d) = %v after the failure, want FetchIfNeeded: a failure releases nothing", idx, got)
			}
			if got := j.Par2ReleaseReason(); got != "" {
				t.Fatalf("Par2ReleaseReason = %q after the failure, want empty", got)
			}
			if err := application.recorder.flush(t.Context()); err != nil {
				t.Fatalf("flush: %v", err)
			}
			assertStoredFetchPolicy(t, application, j.ID(), idx, job.FetchIfNeeded)

			if n, err := application.releaseRecoveryVolumes(j, "test release", job.Unsized()); err != nil || n != 1 {
				t.Fatalf("releaseRecoveryVolumes = %d, %v, want 1, nil", n, err)
			}
			if err := application.recorder.flush(t.Context()); err != nil {
				t.Fatalf("flush: %v", err)
			}
			assertStoredFetchPolicy(t, application, j.ID(), idx, job.FetchAlways)
		})
	}
}
