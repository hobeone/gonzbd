package app

import (
	"hash/crc32"
	"os"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestResumeAllJobs_ClearedCompleteSurvivesRehydration: a file hydrated as
// Complete whose bytes the resume sweep finds missing comes back Outstanding,
// and stays that way across an eviction. Hydration re-applies job_files
// unconditionally, so the sweep's cleared Complete has to reach that row before
// the job can be evicted; left on disk as complete = 1, it hides the file's
// articles from dispatch for good.
func TestResumeAllJobs_ClearedCompleteSurvivesRehydration(t *testing.T) {
	t.Parallel()
	f := newResumeUnitFixture(t)
	ctx := t.Context()
	id := f.job.ID()

	// File 0 finished: both articles durable, job_files.complete = 1.
	commitRuns(t, realStore(t, f.app), id, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 1, Offset: unitArtLen, Length: unitArtLen,
			CRC32: crc32.ChecksumIEEE(f.articles[1])},
	})
	m, err := f.job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if err := seedJobFiles(ctx, f.app.durable, id, m.NumFiles(), f.job.FileFetchPolicy); err != nil {
		t.Fatalf("seedJobFiles: %v", err)
	}
	if _, err := f.repo.DB().ExecContext(ctx,
		`UPDATE job_files SET complete = 1, filename = ? WHERE job_id = ? AND file_index = 0`,
		unitFileOne, id); err != nil {
		t.Fatalf("mark file 0 complete: %v", err)
	}
	if err := writeJobManifest(f.app.config.GetGeneral().AdminDir, f.job); err != nil {
		t.Fatalf("writeJobManifest: %v", err)
	}
	f.app.residency.Evict(id)
	if err := f.app.residency.Hydrate(ctx, id); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if p := liveProgress(t, f.app, id); !p.FileComplete(0) || !p.ArticleDone(0) || !p.ArticleDone(1) {
		t.Fatal("fixture: file 0 did not hydrate as complete with both articles done, " +
			"so the sweep has nothing to clear")
	}

	// Its bytes are gone: the file is shorter than either run claims.
	if err := os.Truncate(f.path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.app.resumeAllJobs(ctx); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}
	if liveProgress(t, f.app, id).FileComplete(0) {
		t.Fatal("fixture: the sweep did not clear file 0's Complete in memory")
	}

	f.app.residency.Evict(id)
	if err := f.app.residency.Hydrate(ctx, id); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	j, ok := f.app.dispatcher.Job(id)
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if j.Progress().FileComplete(0) {
		t.Error("file 0 is Complete again after re-hydration: the sweep's cleared Complete " +
			"never reached job_files, so hydration re-applied the stale complete = 1")
	}
	var outstanding []int32
	j.ForEachUnfinishedArticle(func(_ int, artIdx int32, _ string, _ int, _ int, _ string) bool {
		outstanding = append(outstanding, artIdx)
		return true
	})
	for _, want := range []int32{0, 1} {
		if !slices.Contains(outstanding, want) {
			t.Errorf("article %d of file 0 is not outstanding after re-hydration (outstanding = %v); "+
				"its bytes are gone and nothing will fetch it", want, outstanding)
		}
	}
}
