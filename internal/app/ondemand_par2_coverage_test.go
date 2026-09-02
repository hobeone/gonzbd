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

// This file closes the coverage gap check_coverage reported against
// maybeReleaseRecoveryVolumes after the par2Outcome split: several error and
// switch arms were exercised by nothing. Each test below targets one branch
// that was previously at 0% and asserts real, observable behaviour on that
// branch — not merely that the call does not panic.

// TestMaybeReleaseRecoveryVolumes_NoIndexFindError covers the sErr != nil arm
// of the "no usable par2 index" switch case (app.go's
// `if sErr != nil { reason = fmt.Sprintf(...) }`), which was previously only
// reached via the sErr == nil / len(sets) == 0 side. par2.FindPar2Files
// returns a non-nil error only when os.ReadDir(dir) itself fails, so this
// leaves the job's directory entirely unwritten rather than empty.
func TestMaybeReleaseRecoveryVolumes_NoIndexFindError(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "no-index-readdir-error"
	q := storeBackedQueue(t)
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "no-index-readdir-error-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	seedFileCRC(t, q, qjob, 0, 0x1068AFA6)

	snap := q.SnapshotJob(jobID)
	if snap == nil || !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the snapshot must arrive with a deferred volume")
	}

	// Deliberately do not create downloadDir/no-index-readdir-error-name at
	// all, so par2.FindPar2Files's os.ReadDir(dir) fails with ENOENT instead
	// of succeeding against an empty directory.

	var logBuf bytes.Buffer
	app := &Application{
		queue:   q,
		log:     slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	if !app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
		t.Fatal("maybeReleaseRecoveryVolumes returned false when the par2 index could not even be searched for; repair must be assumed")
	}
	after := q.SnapshotJob(jobID)
	if after.HasDeferredPar2() {
		t.Error("the recovery volume is still deferred after a failed index search; it should have been un-deferred for repair")
	}
	reason := after.Progress().Par2ReleaseReason()
	if !strings.Contains(reason, "err:") {
		t.Errorf("release reason = %q, want it to carry the ReadDir error rather than the no-error fallback message", reason)
	}
}

// TestMaybeReleaseRecoveryVolumes_CleanDiscardFails covers the
// DiscardDeferredPar2 failure branch inside case outcomeClean. That call can
// only fail with ErrNotFound (queue.go's SetPar2ReleaseReason/
// DiscardDeferredPar2/UndeferRecoveryVolumes all key on q.byID), so the job
// is removed from the queue after the snapshot maybeReleaseRecoveryVolumes
// receives was taken — the snapshot is a clone and stays usable, but the
// queue calls the function makes against jobID now see no such job.
func TestMaybeReleaseRecoveryVolumes_CleanDiscardFails(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "clean-discard-fails"
	q := storeBackedQueue(t)
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "clean-discard-fails-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	seedFileCRC(t, q, qjob, 0, 0x1068AFA6)

	snap := q.SnapshotJob(jobID)
	if snap == nil || !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the snapshot must arrive with a deferred volume")
	}

	jobDir := filepath.Join(downloadDir, "clean-discard-fails-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	// The job is gone by the time maybeReleaseRecoveryVolumes acts on the
	// clean verdict it computes from the (still-present) files on disk.
	if err := q.Remove(jobID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var logBuf bytes.Buffer
	app := &Application{
		queue:   q,
		log:     slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
		t.Error("maybeReleaseRecoveryVolumes must return false on a clean verdict even when the discard call fails")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "discarding the deferred volumes did not fully succeed") {
		t.Errorf("the failed discard was not reported:\n%s", logged)
	}
}

// TestMaybeReleaseRecoveryVolumes_Unknown covers case outcomeUnknown end to
// end: nothing on disk matches any par2 entry by name or by content, so the
// job's recovery volumes must be neither discarded nor un-deferred, and the
// release reason must record why. par2Outcome's whole outcomeUnknown arm had
// 0% coverage before this — the earlier tests only reached outcomeClean and
// outcomeRepair.
func TestMaybeReleaseRecoveryVolumes_Unknown(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "unknown-outcome"
	q := storeBackedQueue(t)
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "unknown-outcome-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap := q.SnapshotJob(jobID)
	if snap == nil || !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the snapshot must arrive with a deferred volume")
	}

	jobDir := filepath.Join(downloadDir, "unknown-outcome-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// The par2 index protects "data.bin", but nothing named or shaped like it
	// is delivered: only an unrelated file lands in the directory. Name
	// matching fails (different basename) and content matching fails (the
	// bytes share no Hash16k/CRC32 with the protected entry), so
	// Identification ends with zero id.Files and a non-empty Unaccounted —
	// exactly the outcomeUnknown signature, confirmed empirically against
	// this fixture before writing this test.
	copyFixturePar2(t, jobDir)
	if err := os.WriteFile(filepath.Join(jobDir, "garbage.bin"),
		[]byte("totally unrelated content that shares no hash with the protected entry, padding padding"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	app := &Application{
		queue:   q,
		log:     slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
		t.Error("maybeReleaseRecoveryVolumes must return false on an unidentifiable download; nothing was un-deferred to fetch")
	}
	after := q.SnapshotJob(jobID)
	if !after.HasDeferredPar2() {
		t.Error("the recovery volume must stay deferred (held, not discarded) when nothing on disk could be identified")
	}
	if reason := after.Progress().Par2ReleaseReason(); reason == "" {
		t.Error("the volumes were held without recording why")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "nothing on disk matched the par2 index; holding the volumes") {
		t.Errorf("the held-and-finalizing message was not logged:\n%s", logged)
	}
}

// TestMaybeReleaseRecoveryVolumes_Unknown_ReleaseReasonFails covers the
// SetPar2ReleaseReason failure branch inside case outcomeUnknown, using the
// same job-removed-after-snapshot technique as the clean-discard-fails test
// above.
func TestMaybeReleaseRecoveryVolumes_Unknown_ReleaseReasonFails(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "unknown-outcome-reason-fails"
	q := storeBackedQueue(t)
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "unknown-outcome-reason-fails-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap := q.SnapshotJob(jobID)
	if snap == nil || !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the snapshot must arrive with a deferred volume")
	}

	jobDir := filepath.Join(downloadDir, "unknown-outcome-reason-fails-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	if err := os.WriteFile(filepath.Join(jobDir, "garbage.bin"),
		[]byte("totally unrelated content that shares no hash with the protected entry, padding padding"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := q.Remove(jobID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var logBuf bytes.Buffer
	app := &Application{
		queue:   q,
		log:     slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
		t.Error("maybeReleaseRecoveryVolumes must return false on outcomeUnknown even when recording the reason fails")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "could not record the release reason") {
		t.Errorf("the failed SetPar2ReleaseReason call was not reported:\n%s", logged)
	}
	if !strings.Contains(logged, "nothing on disk matched the par2 index; holding the volumes") {
		t.Errorf("the function must still report holding the volumes even though recording the reason failed:\n%s", logged)
	}
}

// TestMaybeReleaseRecoveryVolumes_RepairReleaseReasonFails covers both
// SetPar2ReleaseReason failure sites inside case outcomeRepair: the one
// taken immediately on entering the case, and the nested one taken after
// UndeferRecoveryVolumes itself also fails. Removing the job after the
// snapshot makes every one of the three queue calls this branch performs
// (SetPar2ReleaseReason, UndeferRecoveryVolumes, SetPar2ReleaseReason again)
// fail with ErrNotFound, which is the only way any of them can fail.
func TestMaybeReleaseRecoveryVolumes_RepairReleaseReasonFails(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "repair-reason-fails"
	q := storeBackedQueue(t)
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "repair-reason-fails-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// A CRC that does not match the fixture's protected data.bin, so
	// verification lands on outcomeRepair via a genuine mismatch rather than
	// a synthesized one.
	seedFileCRC(t, q, qjob, 0, 0xDEADBEEF)

	snap := q.SnapshotJob(jobID)
	if snap == nil || !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the snapshot must arrive with a deferred volume")
	}

	jobDir := filepath.Join(downloadDir, "repair-reason-fails-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	if err := q.Remove(jobID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var logBuf bytes.Buffer
	app := &Application{
		queue:   q,
		log:     slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
		t.Error("maybeReleaseRecoveryVolumes must return false when the job is gone by the time repair is acted on")
	}
	logged := logBuf.String()
	if got := strings.Count(logged, "could not record the release reason"); got != 2 {
		t.Errorf("expected SetPar2ReleaseReason to fail and be logged twice (before and after the un-defer attempt), got %d occurrences:\n%s", got, logged)
	}
	if !strings.Contains(logged, "un-defer failed; finalizing without recovery volumes") {
		t.Errorf("the failed UndeferRecoveryVolumes call was not reported:\n%s", logged)
	}
}
