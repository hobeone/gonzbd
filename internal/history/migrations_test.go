package history

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// openMigratedTestDB opens a scratch SQLite database under t.TempDir() and
// applies the embedded migrations the same way Open does, returning the raw
// *sql.DB so a test can interrogate sqlite_master and the pragma functions
// directly. Open's *DB wrapper keeps its handle unexported and offers no
// schema accessor, and repository_test.go's openTestDB returns
// (*DB, *Repository), so neither is usable for schema-shape assertions.
func openMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "migrations_test.db") + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close scratch db: %v", err)
		}
	})

	subFS, err := fs.Sub(embedMigrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, subFS)
	if err != nil {
		t.Fatalf("new goose provider: %v", err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return db
}

func TestMigrations_SchemaShape(t *testing.T) {
	db := openMigratedTestDB(t)

	t.Run("job_files carries no derived columns", func(t *testing.T) {
		forbidden := []string{"bytes_downloaded", "failed_bytes", "max_written", "write_cursor"}
		rows, err := db.Query(`SELECT name FROM pragma_table_info('job_files')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var cols []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		for _, f := range forbidden {
			if slices.Contains(cols, f) {
				t.Errorf("job_files still has derived column %q — S5 forbids a second authoritative copy", f)
			}
		}
	})

	t.Run("article_facts is keyed for idempotent append", func(t *testing.T) {
		var stmt string
		err := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='table' AND name='article_facts'`,
		).Scan(&stmt)
		if err != nil {
			t.Fatalf("article_facts table missing: %v", err)
		}
		if !strings.Contains(stmt, "PRIMARY KEY") {
			t.Error("article_facts needs a primary key on (job_id, art_idx) for idempotent Append")
		}
	})

	t.Run("file_extents exists with the validity stamp", func(t *testing.T) {
		for _, col := range []string{"durable_bitmap", "verified_to", "prefix_crc", "has_prefix_crc", "bytes_durable", "bytes_failed", "size", "mod_time_ns"} {
			var n int
			err := db.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('file_extents') WHERE name = ?`, col,
			).Scan(&n)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("file_extents missing column %q", col)
			}
		}
	})

	t.Run("only one migration file remains", func(t *testing.T) {
		entries, err := os.ReadDir("migrations")
		if err != nil {
			t.Fatal(err)
		}
		var sqls []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sql") {
				sqls = append(sqls, e.Name())
			}
		}
		if len(sqls) != 1 || sqls[0] != "001_initial.sql" {
			t.Errorf("migrations = %v, want exactly [001_initial.sql]", sqls)
		}
	})
}
