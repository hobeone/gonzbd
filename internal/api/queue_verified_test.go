package api

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// restoredUnverifiedJob builds a two-file job the way a restart does: file
// table and progress seeded from the record, every file unverified, articles
// attached so a file can then be verified.
func restoredUnverifiedJob(t *testing.T) (*job.Job, dispatch.Row) {
	t.Helper()
	m := job.NewManifest([]job.JobFile{
		{Subject: "a.bin", Bytes: 200, Articles: []job.JobArticle{{ID: "<a0@x>", Bytes: 100}, {ID: "<a1@x>", Bytes: 100}}},
		{Subject: "b.bin", Bytes: 100, Articles: []job.JobArticle{{ID: "<b0@x>", Bytes: 100}}},
	})
	j := job.New("v1", "v.nzb", job.Policy{})
	seeds := []job.FileSeed{{Filename: "a.bin"}, {Filename: "b.bin"}}
	if err := j.AttachFiles(m.Files(), seeds); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	if err := j.AttachArticles(m); err != nil {
		t.Fatalf("AttachArticles: %v", err)
	}
	row := dispatch.Row{
		ID:     "v1",
		Header: dispatch.Header{Name: "v.nzb", Filename: "v.nzb", Bytes: m.Files().TotalBytes()},
		View:   job.RenderView{StateView: job.StateView{State: job.Fetching}},
	}
	return j, row
}

// A restored job's figures are an estimate until each file is verified, and
// the slot must say so.
func TestBuildSlot_ProgressVerified(t *testing.T) {
	t.Parallel()
	j, row := restoredUnverifiedJob(t)
	if j.AllVerified() {
		t.Fatal("fixture guard: seeded job reports AllVerified")
	}
	if slot := buildSlot(row, j, false, 0, 0, nil, ""); slot.ProgressVerified {
		t.Error("ProgressVerified = true for a job whose files are all unverified")
	}
	for fi := range 2 {
		if _, err := j.InstallFileVerification(job.FileVerification{FileIdx: fi}); err != nil {
			t.Fatalf("InstallFileVerification(%d): %v", fi, err)
		}
	}
	if slot := buildSlot(row, j, false, 0, 0, nil, ""); !slot.ProgressVerified {
		t.Error("ProgressVerified = false after every file was verified")
	}
}

// One unverified file is enough to keep the slot unverified.
func TestBuildSlot_ProgressVerified_PartiallyVerified(t *testing.T) {
	t.Parallel()
	j, row := restoredUnverifiedJob(t)
	if _, err := j.InstallFileVerification(job.FileVerification{FileIdx: 0}); err != nil {
		t.Fatalf("InstallFileVerification: %v", err)
	}
	if slot := buildSlot(row, j, false, 0, 0, nil, ""); slot.ProgressVerified {
		t.Error("ProgressVerified = true with file 1 still unverified")
	}
}

// A freshly ingested job (nil seed) has every file verified.
func TestBuildSlot_ProgressVerified_FreshJob(t *testing.T) {
	t.Parallel()
	m := job.NewManifest([]job.JobFile{{Subject: "x", Bytes: 10, Articles: []job.JobArticle{{ID: "<x@x>", Bytes: 10}}}})
	j := job.New("f1", "f.nzb", job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	row := dispatch.Row{ID: "f1", Header: dispatch.Header{Name: "f.nzb", Bytes: 10}}
	if slot := buildSlot(row, j, false, 0, 0, nil, ""); !slot.ProgressVerified {
		t.Error("ProgressVerified = false for a freshly ingested job")
	}
}

// A registered job whose job_files could not be restored has no progress; its
// figure is unchecked, so it reads unverified (the tick fails it at first
// hydration).
func TestBuildSlot_ProgressVerified_NoProgress(t *testing.T) {
	t.Parallel()
	j := job.New("np", "np.nzb", job.Policy{})
	row := dispatch.Row{ID: "np", Header: dispatch.Header{Name: "np.nzb", Bytes: 300}}
	if slot := buildSlot(row, j, false, 0, 0, nil, ""); slot.ProgressVerified {
		t.Error("ProgressVerified = true for a job with no progress record")
	}
	if got := buildQueueFiles(j); len(got) != 0 {
		t.Errorf("buildQueueFiles = %d entries for a job with no progress, want none", len(got))
	}
}

// A row whose job is already gone is being removed; nothing is left to verify.
func TestBuildSlot_ProgressVerified_JobGone(t *testing.T) {
	t.Parallel()
	row := dispatch.Row{ID: "gone", Header: dispatch.Header{Name: "g.nzb", Bytes: 300}}
	if slot := buildSlot(row, nil, false, 0, 0, nil, ""); !slot.ProgressVerified {
		t.Error("ProgressVerified = false for a row with no job")
	}
}

func TestBuildQueueFiles_ReportsPerFileVerified(t *testing.T) {
	t.Parallel()
	j, _ := restoredUnverifiedJob(t)
	got := buildQueueFiles(j)
	if len(got) != 2 || got[0].Verified || got[1].Verified {
		t.Fatalf("before verification = %+v, want two unverified files", got)
	}
	if _, err := j.InstallFileVerification(job.FileVerification{FileIdx: 1}); err != nil {
		t.Fatalf("InstallFileVerification: %v", err)
	}
	got = buildQueueFiles(j)
	if got[0].Verified || !got[1].Verified {
		t.Errorf("after verifying file 1 = %+v, want only file 1 verified", got)
	}
}
