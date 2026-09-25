package durability

import (
	"context"
	"slices"
	"testing"
)

// TestSaveProgress_RefusesAFailedArticleForAJobWithNoFiles pins the liveness
// guard: a checkpoint batch captured before a departure commits after the
// reclaim took the job's rows, and must not put them back (#561).
//
// job_files is the marker rather than dispatch_jobs: Admit seeds it before the
// job is added, so it is true for every live job, and a dispatch_jobs guard
// would drop legitimate writes for a job between launch and its first persist.
func TestSaveProgress_RefusesAFailedArticleForAJobWithNoFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewStore(openTestDB(t))

	// Live: the article is recorded. Without this the test below would pass
	// against a SaveProgress that never writes a failed article at all.
	if err := st.Admit(ctx, "live", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProgress(ctx, []JobProgress{{JobID: "live", FailedArticles: []int{7}}}); err != nil {
		t.Fatalf("SaveProgress for a live job: %v", err)
	}
	got, err := st.FailedArticles(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int32{7}) {
		t.Fatalf("live job's failed articles = %v, want [7]; the guard is refusing a "+
			"write it must allow, and every downloaded job loses its failure marks", got)
	}

	// Departed: Admit, then reclaim it the way a departure does — nothing
	// reaches the job, so job_files goes with everything else.
	if err := st.Admit(ctx, "departed", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reclaim(ctx, "departed"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProgress(ctx, []JobProgress{{JobID: "departed", FailedArticles: []int{7}}}); err != nil {
		t.Fatalf("SaveProgress for a departed job: %v", err)
	}
	got, err = st.FailedArticles(ctx, "departed")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a departed job kept %v after a late flush; nothing reaches this job, so "+
			"these rows are unreachable for the life of the installation and a retry of the "+
			"same id would read them as permanent failures", got)
	}
}

// TestSaveProgress_StillWritesFileRowsForADepartedJob pins the guard's scope.
// The UPDATE is left unguarded on purpose: it matches no row once the reclaim
// has run, so it cannot resurrect anything, and guarding it would put a second
// liveness decision in the same statement list.
func TestSaveProgress_StillWritesFileRowsForADepartedJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewStore(openTestDB(t))

	if err := st.Admit(ctx, "departed", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reclaim(ctx, "departed"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProgress(ctx, []JobProgress{{
		JobID: "departed",
		Files: []FileRow{{FileIndex: 0, Filename: "x", FetchPolicy: 2}},
	}}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	rows, err := st.FileRows(ctx, "departed")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("job_files rows = %+v, want none: the UPDATE matched a row that the "+
			"reclaim was supposed to have taken", rows)
	}
}
