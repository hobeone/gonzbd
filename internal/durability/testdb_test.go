package durability

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // driver for the scratch database below

	"github.com/hobeone/gonzbd/internal/history"
)

// openTestDB creates a migrated, empty database for one test.
//
// It goes through history.Open rather than executing DDL of its own, so the
// schema under test is always the one the migrations produce. It lived in
// factlog_sqlite_test.go until article_facts was dropped; it is here now
// because it belongs to no single store's tests.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	hdb, err := history.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	if err := hdb.Close(); err != nil {
		t.Fatalf("close migration handle: %v", err)
	}

	dsn := path + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close scratch db: %v", err)
		}
	})
	return db
}
