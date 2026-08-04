package history_test

import (
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
)

// TestEntry_NZBBackupRoundTrip pins that the NZB backup basename survives an
// Add/Get round trip through the history table. Retry re-parses
// admin/nzb/<basename> to recover the article message-IDs, so an entry that
// loses this field is unretryable while looking entirely intact.
func TestEntry_NZBBackupRoundTrip(t *testing.T) {
	db, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	entry := history.Entry{
		NzoID:     "nzbbackuphist001",
		Name:      "backup-entry",
		NzbName:   "Show.S01E01.nzb",
		NZBBackup: "Show.S01E01.nzb.1.gz",
		Status:    "Failed",
	}
	if err := repo.Add(t.Context(), entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := repo.Get(t.Context(), entry.NzoID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.NZBBackup != "Show.S01E01.nzb.1.gz" {
		t.Errorf("NZBBackup = %q, want %q", got.NZBBackup, "Show.S01E01.nzb.1.gz")
	}
	// NzbName is a compatibility surface (mode=history JSON, the UI history
	// row, the post-processing script's $2, the search predicate). The two
	// diverge whenever a forced duplicate add takes a suffix, so a shared
	// column would corrupt one of them.
	if got.NzbName != "Show.S01E01.nzb" {
		t.Errorf("NzbName = %q, want %q", got.NzbName, "Show.S01E01.nzb")
	}
}
