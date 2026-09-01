//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestIntegration_DuplicateDetection covers the three duplicate triggers:
// same filename, same MD5 under a different filename, and the NZB backup each
// one leaves behind.
//
// PAST FLAKE — read this before assuming a failure here is your change.
//
// This test spent part of #298 failing at ~14% (13 runs in 90) with
// "database is locked" out of app.AddJob → sqlite store insert job, both as
// plain SQLITE_BUSY (5) and BUSY_SNAPSHOT (517). It is fixed, but the shape
// is worth keeping written down, because this test is unusually good at
// provoking it and will be the first to fail if it comes back.
//
// What it was. SQLiteStore.Add opened a deferred transaction, read
// SELECT MAX(sort_key), and then wrote. Under WAL that has to upgrade to the
// write lock, and if another connection holds it SQLite returns
// SQLITE_BUSY immediately rather than invoking the busy handler — waiting
// while already holding a read snapshot could deadlock. So busy_timeout(5000)
// in internal/history/db.go never applied to it, and the only recourse was
// to restart the transaction. Add also wrote the manifest file, and its
// fsync, inside that transaction, which held the write lock for as long as
// the disk took.
//
// Why here. The first job below downloads, fails, and finalizes through
// MoveToHistory while jobs two and three are being added — a genuine
// concurrent commit, not test scaffolding. #298 then gave every add an NZB
// backup write, whose fsync widened the gap between Add's read and its
// write enough to lose the race regularly. Nothing about the failure was
// specific to duplicate detection; this test just has three sequential adds
// racing a live pipeline.
//
// What fixed it: _txlock=immediate in internal/history/db.go, which takes
// the write lock at BEGIN so there is no upgrade to contend on and
// busy_timeout finally applies, plus Add writing the manifest before the
// transaction opens rather than holding the write lock across its fsync.
// Measured 0 in 40 runs afterwards, from 13 in 90 before, with
// internal/queue's TestSQLiteStore_AddSurvivesConcurrentCommits pinning the
// property directly.
//
// A retry loop was tried and removed. It was not merely redundant once the
// DSN change landed but actively harmful: contention moved to BeginTx, which
// blocks for the full busy_timeout (5.01s measured) rather than failing
// fast, so retrying multiplied the wait instead of avoiding it — 48.8s for
// ten attempts, all of it under q.mu, which Queue.Add holds across both
// store.Add and store.ShiftSortKey. See withWriteTx's doc comment.
//
// If it returns: reproduce it standalone in a loop rather than trusting a
// single run — an early read of this as "only under full-suite load" came
// from a lucky streak of isolated passes and was wrong. Then check whether
// a new read-then-write transaction has been added without withWriteTx.
func TestIntegration_DuplicateDetection(t *testing.T) {
	srv := newMockServer(t, nil)
	// We need a stable directory to check the admin/nzb folder
	dir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), dir)

	payload := []byte("dummy content")
	files := []TestFile{{Name: "duplicate.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	// 1. Add first NZB
	addNZBJob(t, a, rawNZB, "duplicate")

	// Verify NZB backup exists under the original filename.
	//
	// newTestAppWithDir sets AdminDir = downloadDir (dir here), and AddJob
	// backs up under filepath.Join(app.cfg.AdminDir, "nzb") -- see
	// buildAppConfig in testhelpers_test.go.
	nzbBackupDir := filepath.Join(dir, "nzb")
	backupPath := filepath.Join(nzbBackupDir, "duplicate.nzb.gz")
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("NZB gzip backup missing: %v", err)
	}

	// 2. Add same NZB again (same filename trigger)
	job2 := addNZBJob(t, a, rawNZB, "duplicate")

	// 3. Verify it is paused and has duplicate warning
	if job2.Status != constants.StatusPaused {
		t.Errorf("duplicate job status = %q, want Paused", job2.Status)
	}
	if job2.Warning != "Duplicate NZB" {
		t.Errorf("duplicate job warning = %q, want 'Duplicate NZB'", job2.Warning)
	}

	// A duplicate now gets a backup of its own, under a suffixed name.
	//
	// This reverses the earlier behaviour of skipping the write. The backup
	// is the only surviving copy of the article message-IDs once a job's
	// manifest is unlinked at finalization, so a job without one cannot be
	// retried — and a non-forced duplicate is enqueued paused, meaning the
	// operator can resume it, download it, and have it fail. The original
	// file is left intact rather than overwritten, because admin/nzb/ is
	// browsed by hand for the name a job was submitted under.
	if job2.NZBBackup == "" {
		t.Error("duplicate job recorded no NZB backup, so it cannot be retried if resumed")
	}
	if job2.NZBBackup == "duplicate.nzb.gz" {
		t.Error("duplicate job reused the first job's backup name; the original would be overwritten")
	}
	if _, err := os.Stat(filepath.Join(nzbBackupDir, job2.NZBBackup)); err != nil {
		t.Errorf("duplicate job's backup %q missing: %v", job2.NZBBackup, err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("first job's backup was clobbered by the duplicate: %v", err)
	}

	// 4. Add same NZB again with DIFFERENT filename (MD5 trigger).
	// Still detected as a duplicate, still paused.
	job3 := addNZBJob(t, a, rawNZB, "duplicate-renamed")
	if job3.Status != constants.StatusPaused {
		t.Errorf("MD5 duplicate job status = %q, want Paused", job3.Status)
	}

	// Same reasoning as above: paused, but resumable, so it needs a backup.
	// This one takes its own filename with no suffix, since nothing else
	// claims it.
	if job3.NZBBackup != "duplicate-renamed.nzb.gz" {
		t.Errorf("MD5 duplicate NZBBackup = %q, want %q", job3.NZBBackup, "duplicate-renamed.nzb.gz")
	}
	if _, err := os.Stat(filepath.Join(nzbBackupDir, "duplicate-renamed.nzb.gz")); err != nil {
		t.Errorf("MD5 duplicate's backup missing: %v", err)
	}
}

func TestIntegration_DirectoryCollision(t *testing.T) {
	srv := newMockServer(t, nil)
	dir := t.TempDir()

	// Pre-create a directory that would collide
	collidingDir := filepath.Join(dir, "collision")
	if err := os.MkdirAll(collidingDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	a := newTestAppWithDir(t, srv.Addr(), dir)

	payload := []byte("dummy content")
	files := []TestFile{{Name: "data.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	// Add job with name "collision"
	job := addNZBJob(t, a, rawNZB, "collision")

	// Verify job was renamed to collision.1
	if job.Name != "collision.1" {
		t.Errorf("job name = %q, want collision.1", job.Name)
	}
}
