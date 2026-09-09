package postproc

import (
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// buildRestoredTestJob builds a job the way dispatch.restore does: the stored
// par2 state is applied BEFORE any content exists, and hydration seeds the
// fresh JobProgress from it.
//
// The ordering is the whole point. buildTestJob attaches content first and the
// other subtests call SetPar2ReleaseReason afterwards, which is the ordinary
// runtime write path — a pin built that way reports the same arm before and
// after #504's fix and so discriminates nothing.
func buildRestoredTestJob(t *testing.T, reason string, recovered bool) *job.Job {
	t.Helper()
	files := []job.JobFile{
		{Subject: "release.rar", Bytes: 500, Articles: []job.JobArticle{
			{ID: "d0@t", Bytes: 500, Number: 1},
		}},
		{Subject: "x.vol000+01.par2", Bytes: 500, IsPar2Recovery: true, Deferred: true,
			Articles: []job.JobArticle{{ID: "p0@t", Bytes: 500, Number: 1}}},
	}
	job.SortJobFiles(files)

	j := job.New("restored-id", "restored-name", job.Policy{})
	j.RestoreProgressState(reason, time.Unix(1700000100, 0).UTC(), time.Unix(1700000200, 0).UTC(), recovered)

	m := job.NewManifest(files)
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	// A job whose volumes were un-deferred before the restart has them at
	// FetchAlways, which job_files.fetch_policy carries across restarts; one
	// whose verdict released nothing still holds them.
	if !recovered {
		for fi := range m.NumFiles() {
			if m.FileIsPar2Recovery(fi) {
				if err := j.SetFileFetchPolicy(fi, job.FetchIfNeeded); err != nil {
					t.Fatalf("SetFileFetchPolicy: %v", err)
				}
			}
		}
	}
	return j
}

// TestBuildDownloadFileList_RestoredVerdictIsNotReportedClean pins the
// user-visible half of #504's first commit.
//
// A job whose verdict released nothing and whose volumes are still held takes
// arm C1 ("could not verify") only while HasPar2Verdict() is true, and that
// reads the release reason. Before the fix the reason did not survive a
// restart, so HasPar2Verdict() was false, C1 was skipped, and the job fell
// through to the bare heldVols arm — reporting "verified clean" for a job
// whose par2 verdict had found it unverifiable.
func TestBuildDownloadFileList_RestoredVerdictIsNotReportedClean(t *testing.T) {
	qjob := buildRestoredTestJob(t, "no delivered file matched any par2 entry", false)

	if !qjob.Progress().HasPar2Verdict() {
		t.Fatal("fixture guard: the restored reason must survive hydration, or this pins nothing")
	}
	if qjob.Progress().Par2Recovered() {
		t.Fatal("fixture guard: Par2Recovered must be false for the could-not-verify arm")
	}

	got := strings.Join(buildDownloadFileList(&Job{DownloadDir: t.TempDir(), Job: qjob}), "\n")

	if strings.Contains(got, "verified clean") {
		t.Errorf("a restored job whose verdict could not verify must not report verified clean; got:\n%s", got)
	}
	if !strings.Contains(got, "could not verify") {
		t.Errorf("expected the could-not-verify line for a restored held-volume job; got:\n%s", got)
	}
	if !strings.Contains(got, "no delivered file matched any par2 entry") {
		t.Errorf("expected the restored release reason in the output; got:\n%s", got)
	}
}

// TestBuildDownloadFileList_RepairedJobReportsFetchedVolumesAfterRestart pins
// the symptom #504 reports.
//
// A job repaired before a restart has every recovery volume at FetchAlways, so
// heldVols is 0 and neither held-volume arm applies; the only arm that can
// speak for it tests Par2Recovered(). While that flag had no durable home it
// came back false, so NO arm matched and the summary was silently absent —
// not a wrong line, no line at all, with nothing to indicate the job had
// needed repair.
func TestBuildDownloadFileList_RepairedJobReportsFetchedVolumesAfterRestart(t *testing.T) {
	qjob := buildRestoredTestJob(t, "repair needed", true)

	if !qjob.Progress().Par2Recovered() {
		t.Fatal("fixture guard: the restored par2Recovered flag must survive hydration, or this pins nothing")
	}
	if qjob.Progress().HasDeferredPar2() {
		t.Fatal("fixture guard: a repaired job holds no volumes, so heldVols must be 0")
	}

	got := strings.Join(buildDownloadFileList(&Job{DownloadDir: t.TempDir(), Job: qjob}), "\n")

	if !strings.Contains(got, "⚠ Par2: fetched 1 recovery volume(s) for repair (reason: repair needed)") {
		t.Errorf("a repaired job must report its fetched recovery volumes after a restart; got:\n%s", got)
	}
	if strings.Contains(got, "verified clean") {
		t.Errorf("a repaired job must not report verified clean; got:\n%s", got)
	}
	if strings.Contains(got, "could not verify") {
		t.Errorf("a repaired job reached a verdict and acted on it; got:\n%s", got)
	}
}
