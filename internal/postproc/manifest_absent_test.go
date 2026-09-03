package postproc

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/par2"
)

// evictedJob returns a postproc Job whose lifecycle job has no resident
// manifest, the state a job reaches when its content tier has been evicted
// (a paused, non-active job on the real path; here, directly via Evict()).
//
// It attaches a manifest with a real file before evicting: NumFiles() reads
// the scalar AttachContent set, which survives Evict() (see
// internal/job/content.go's Evict doc comment -- only the manifest pointer is
// dropped). recordVerdict's own guard short-circuits at NumFiles()==0 before
// ever reaching the Manifest() call this fixture exists to exercise, so a
// job built from an empty manifest (like newQueueJob's) would make that
// guard fire first and leave the manifest-unreadable path untested.
func evictedJob(t *testing.T) *Job {
	t.Helper()
	j := job.New("evicted", "evicted", job.Policy{})
	m := buildManifest(t, []testFile{
		{Subject: "movie.part01.rar", Bytes: 100, Articles: []testArticle{{ID: "a0@t", Bytes: 100, Number: 1}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("evictedJob: AttachContent: %v", err)
	}
	j.Evict()
	if _, mErr := j.Manifest(); mErr == nil {
		t.Fatal("fixture guard: manifest still resident, nothing is being tested")
	}
	if j.NumFiles() == 0 {
		t.Fatal("fixture guard: NumFiles must survive eviction, or recordVerdict's earlier guard would short-circuit this fixture")
	}
	return &Job{Job: j}
}

// The file listing is reporting, so it degrades — but it says why, in the
// record itself. An empty listing and a job that downloaded nothing look
// identical to whoever reads the history entry later.
func TestBuildDownloadFileList_AbsentManifestExplainsItself(t *testing.T) {
	lines := buildDownloadFileList(evictedJob(t))

	if len(lines) == 0 {
		t.Fatal("returned no lines at all; the history record would show an empty download stage with no explanation")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "File listing unavailable") {
		t.Errorf("listing does not explain its own absence:\n%s", joined)
	}
}

// Quickcheck must not report a job as CRC-verified when it verified nothing,
// and must say so in the field downstream reads rather than only in the error
// it returns.
//
// The outcome is the load-bearing part. The error goes into the stage log and
// the history entry, but the repair stage never sees it — it reads
// job.QuickCheck. Reporting "nothing to check here" is what let DirectUnpack's
// success skip par2 for a job nothing had verified (#294).
//
// Run sets Inconclusive before calling this, once it knows par2 sets exist
// (#314), so what this asserts is that the unreadable-manifest path leaves
// that standing rather than narrowing to a verdict it did not earn.
func TestVerifyJobCRCs_AbsentManifestErrorsRatherThanClaimingVerified(t *testing.T) {
	job := evictedJob(t)
	job.QuickCheck = QuickCheckInconclusive
	q := &QuickCheckStage{}

	err := q.recordVerdict(context.Background(), slog.Default(), job, []par2.Set{}, par2.Assessment{})

	if err == nil {
		t.Fatal("recordVerdict returned nil with no manifest; the stage log would record a clean pass over nothing")
	}
	if job.QuickCheck != QuickCheckInconclusive {
		t.Errorf("QuickCheck = %s, want inconclusive: downstream cannot tell a verification that could not run from one that had nothing to run on", job.QuickCheck)
	}
}
