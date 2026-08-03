package queue_test

import (
	"testing"
)

// TestSQLiteStore_NZBBackupRoundTrip pins that the recorded NZB backup
// basename survives a store round trip. Retry resolves admin/nzb/<basename>
// from this field, so a job that loses it on restart becomes unretryable —
// and silently so, because every other field it needs is still intact.
func TestSQLiteStore_NZBBackupRoundTrip(t *testing.T) {
	store, _, _ := setupTestStore(t)

	job := newTestJobWithManifest(t, "nzbbackup0000001", "backup-job", 2, 3)
	job.Filename = "Show.S01E01.nzb"
	job.NZBBackup = "Show.S01E01.nzb.1.gz"

	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := store.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.NZBBackup != "Show.S01E01.nzb.1.gz" {
		t.Errorf("NZBBackup = %q, want %q", got.NZBBackup, "Show.S01E01.nzb.1.gz")
	}
	// Filename must not be overwritten by the backup name: the two diverge
	// whenever a forced duplicate takes a suffix, and Filename is the
	// user-facing submitted name.
	if got.Filename != "Show.S01E01.nzb" {
		t.Errorf("Filename = %q, want %q", got.Filename, "Show.S01E01.nzb")
	}
}

// TestSQLiteStore_NZBBackupDefaultsEmpty pins that a job added without a
// backup name reads back as empty rather than erroring, so the column can be
// added to a database whose rows predate it.
func TestSQLiteStore_NZBBackupDefaultsEmpty(t *testing.T) {
	store, _, _ := setupTestStore(t)

	job := newTestJob("nzbbackup0000002", "no-backup")
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := store.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.NZBBackup != "" {
		t.Errorf("NZBBackup = %q, want empty", got.NZBBackup)
	}
}
