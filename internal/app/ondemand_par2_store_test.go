package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
)

// The on-demand par2 path, end to end, in the configuration production uses.
//
// maybeReleaseRecoveryVolumes returns early on !j.HasDeferredPar2(), so
// with the deferral discarded the entire function was unreachable in any real
// deployment: the verification never ran, the volumes were never released,
// and they had already been downloaded with everything else — the opposite of
// what the feature promises.
func TestMaybeReleaseRecoveryVolumes_WithStore(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()

	app, _, _ := newLifecycleTestApp(t)
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = downloadDir
		c.Downloads.OnDemandPar2 = true
	})

	const jobID = "store-par2"
	qjob, hdr := newPar2Job(t, jobID, "store-par2-name", []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	seedFileCRC(t, qjob, 0, 0x1068AFA6)
	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !qjob.HasDeferredPar2() {
		t.Fatal("the job reports no deferred par2 with a store configured; maybeReleaseRecoveryVolumes returns early and nothing below this line runs")
	}

	jobDir := filepath.Join(downloadDir, "store-par2-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	// Data verifies clean, so the recovery volumes are not needed and must be
	// discarded rather than fetched — the saving the feature exists for.
	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID) {
		t.Error("maybeReleaseRecoveryVolumes returned true on clean data; the recovery volumes would be downloaded for nothing")
	}

	m := mustManifest(t, qjob)
	sawRecovery := false
	for fi := range m.NumFiles() {
		if !m.FileIsPar2Recovery(fi) {
			continue
		}
		sawRecovery = true
		// DiscardDeferredPar2 marks the volume FetchNever and leaves it in place.
		if got := qjob.Progress().FileFetchPolicy(fi); got != job.FetchNever {
			t.Errorf("recovery file %d policy = %d, want FetchNever after a clean verification", fi, got)
		}
	}
	if !sawRecovery {
		t.Fatal("fixture guard: expected a par2 recovery file to still be present in the manifest")
	}
}

// The other half: when the data is damaged the volumes must be un-deferred so
// they are fetched for repair. Also store-backed, for the same reason.
func TestMaybeReleaseRecoveryVolumes_WithStore_CorruptData(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()

	app, _, _ := newLifecycleTestApp(t)
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = downloadDir
		c.Downloads.OnDemandPar2 = true
	})

	const jobID = "store-par2-corrupt"
	qjob, hdr := newPar2Job(t, jobID, "store-par2-corrupt-name", []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	seedFileCRC(t, qjob, 0, 0xDEADBEEF)
	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !qjob.HasDeferredPar2() {
		t.Fatal("fixture guard: the job must arrive with a deferred volume or nothing below runs")
	}

	jobDir := filepath.Join(downloadDir, "store-par2-corrupt-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	if !app.maybeReleaseRecoveryVolumes(t.Context(), jobID) {
		t.Fatal("maybeReleaseRecoveryVolumes returned false on damaged data; the recovery volumes needed for repair are never fetched")
	}
	if qjob.HasDeferredPar2() {
		t.Error("the recovery volume is still deferred after damage was detected, so repair will run without it")
	}
	if reason := qjob.Progress().Par2ReleaseReason(); reason == "" {
		t.Error("the volumes were released without recording why")
	}
}

// The unreadable-manifest branch, which #289 added: a job can arrive with its
// volumes still deferred and its manifest unreadable at once. That combination
// is the branch: the CRC comparison cannot be made, and finalizing would ship
// a download that may need a repair nothing checked for.
func TestMaybeReleaseRecoveryVolumes_UnreadableManifest(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()

	app, _, _ := newLifecycleTestApp(t)
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = downloadDir
		c.Downloads.OnDemandPar2 = true
	})

	const jobID = "store-par2-unreadable"
	qjob, hdr := newPar2Job(t, jobID, "store-par2-unreadable-name", []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Evict the manifest so j.Manifest() returns ErrNotResident
	qjob.Evict()

	if !qjob.HasDeferredPar2() {
		t.Fatal("fixture guard: the deferral did not survive the eviction, so the early return fires")
	}

	var logBuf bytes.Buffer
	app.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if got := app.maybeReleaseRecoveryVolumes(t.Context(), jobID); got {
		t.Errorf("maybeReleaseRecoveryVolumes = true, want false: the un-defer cannot succeed while the job is non-resident")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "cannot verify without the manifest") {
		t.Errorf("the unverifiable manifest was not reported:\n%s", logged)
	}
	if !strings.Contains(logged, "un-defer failed") {
		t.Errorf("the failed un-defer was not reported, so finalizing without recovery volumes happens silently:\n%s", logged)
	}
}
