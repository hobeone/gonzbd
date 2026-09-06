package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// This file closes the coverage gap against maybeReleaseRecoveryVolumes: each test
// below targets one branch and asserts real, observable behaviour on that
// branch — not merely that the call does not panic.

// TestMaybeReleaseRecoveryVolumes_NoIndexFindError covers the sErr != nil arm
// of the "no usable par2 index" switch case (app.go's
// `if sErr != nil { reason = fmt.Sprintf(...) }`), which is reached when
// par2.FindPar2Files returns a non-nil error.
func TestMaybeReleaseRecoveryVolumes_NoIndexFindError(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()

	const jobID = "no-index-readdir-error"
	app := newTestApplication(t)
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = downloadDir
		c.Downloads.OnDemandPar2 = true
	})

	qjob, hdr := newPar2Job(t, jobID, "no-index-readdir-error-name", []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	seedFileCRC(t, qjob, 0, 0x1068AFA6)
	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !qjob.HasDeferredPar2() {
		t.Fatal("fixture guard: the job must arrive with a deferred volume")
	}

	// Deliberately do not create downloadDir/no-index-readdir-error-name at
	// all, so par2.FindPar2Files's os.ReadDir(dir) fails with ENOENT instead
	// of succeeding against an empty directory.
	var logBuf bytes.Buffer
	app.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if !app.maybeReleaseRecoveryVolumes(t.Context(), jobID) {
		t.Fatal("maybeReleaseRecoveryVolumes returned false when the par2 index could not even be searched for; repair must be assumed")
	}
	if qjob.HasDeferredPar2() {
		t.Error("the recovery volume is still deferred after a failed index search; it should have been un-deferred for repair")
	}
	reason := qjob.Progress().Par2ReleaseReason()
	if !strings.Contains(reason, "err:") {
		t.Errorf("release reason = %q, want it to carry the ReadDir error rather than the no-error fallback message", reason)
	}
}

// TestMaybeReleaseRecoveryVolumes_JobGone covers the job-not-found guard.
func TestMaybeReleaseRecoveryVolumes_JobGone(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	if app.maybeReleaseRecoveryVolumes(t.Context(), "absent-job") {
		t.Error("maybeReleaseRecoveryVolumes must return false when the job is not in the dispatcher")
	}
}

// TestMaybeReleaseRecoveryVolumes_Unknown covers case outcomeUnknown end to
// end: nothing on disk matches any par2 entry by name or by content, so the
// job's recovery volumes must be neither discarded nor un-deferred, and the
// release reason must record why.
func TestMaybeReleaseRecoveryVolumes_Unknown(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()

	const jobID = "unknown-outcome"
	app := newTestApplication(t)
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = downloadDir
		c.Downloads.OnDemandPar2 = true
	})

	qjob, hdr := newPar2Job(t, jobID, "unknown-outcome-name", []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !qjob.HasDeferredPar2() {
		t.Fatal("fixture guard: the job must arrive with a deferred volume")
	}

	jobDir := filepath.Join(downloadDir, "unknown-outcome-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	if err := os.WriteFile(filepath.Join(jobDir, "garbage.bin"),
		[]byte("totally unrelated content that shares no hash with the protected entry, padding padding"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	app.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID) {
		t.Error("maybeReleaseRecoveryVolumes must return false on an unidentifiable download; nothing was un-deferred to fetch")
	}
	if !qjob.HasDeferredPar2() {
		t.Error("the recovery volume must stay deferred (held, not discarded) when nothing on disk could be identified")
	}
	if reason := qjob.Progress().Par2ReleaseReason(); reason == "" {
		t.Error("the volumes were held without recording why")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "nothing on disk matched the par2 index; holding the volumes") {
		t.Errorf("the held-and-finalizing message was not logged:\n%s", logged)
	}
}

// TestMaybeReleaseRecoveryVolumes_RepairUndeferFails covers the failure branch
// inside case outcomeRepair when UndeferRecoveryVolumes fails (e.g. because the
// manifest is non-resident).
func TestMaybeReleaseRecoveryVolumes_RepairUndeferFails(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()

	const jobID = "repair-undefer-fails"
	app := newTestApplication(t)
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = downloadDir
		c.Downloads.OnDemandPar2 = true
	})

	qjob, hdr := newPar2Job(t, jobID, "repair-undefer-fails-name", []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	seedFileCRC(t, qjob, 0, 0xDEADBEEF)
	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !qjob.HasDeferredPar2() {
		t.Fatal("fixture guard: the job must arrive with a deferred volume")
	}

	jobDir := filepath.Join(downloadDir, "repair-undefer-fails-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	// Evicting the manifest makes UndeferRecoveryVolumes fail with ErrNotResident.
	qjob.Evict()

	var logBuf bytes.Buffer
	app.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID) {
		t.Error("maybeReleaseRecoveryVolumes must return false when un-defer fails")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "un-defer failed; finalizing without recovery volumes") {
		t.Errorf("the failed UndeferRecoveryVolumes call was not reported:\n%s", logged)
	}
}
