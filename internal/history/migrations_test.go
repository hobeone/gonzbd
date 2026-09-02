package history

import (
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// updateGolden makes TestMigrations_GoldenSchema rewrite
// testdata/schema.golden from the migration instead of asserting against it:
// `go test ./internal/history -run TestMigrations_GoldenSchema -update`.
var updateGolden = flag.Bool("update", false, "rewrite testdata/schema.golden from the migration")

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

// primaryKeyOrdinals returns the table's primary-key columns mapped to their
// 1-based position within the key. A non-key column has pk=0 and is omitted, so
// an empty result means the table has no primary key.
func primaryKeyOrdinals(t *testing.T, db *sql.DB, table string) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT name, pk FROM pragma_table_info(?) WHERE pk > 0`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var name string
		var pk int
		if err := rows.Scan(&name, &pk); err != nil {
			t.Fatal(err)
		}
		out[name] = pk
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrations_SchemaShape(t *testing.T) {
	db := openMigratedTestDB(t)

	t.Run("job_files carries no derived columns", func(t *testing.T) {
		// failed_bytes and bytes_downloaded are deliberately NOT forbidden —
		// see the sibling subtest below. The two named here are values
		// maintained in parallel with the facts they summarise, which is the
		// S5 violation; those two are caches of the same row's articles_done
		// bits, with a single writer.
		//
		// bytes_downloaded was on this list, because the column of that name
		// removed in #306 had a second writer. The name is reused; the shape
		// is not. What makes a column legitimate here is the writer count, not
		// the identifier, so the list has to shrink when the writer does —
		// keeping it would forbid the fix rather than the defect.
		forbidden := []string{"max_written", "write_cursor"}
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

	// job_files.failed_bytes is the one cached figure the durability record
	// cannot supply. failed_articles records WHICH articles failed and never
	// how many bytes they were, and a permanently failed article never decodes
	// so no run covers it either. Both halves are asserted — present here,
	// absent from the record's own tables below — because the pair is the
	// decision. Checking only one half would let a future change satisfy it by
	// adding a byte column to the record as well, which is the two-writer
	// shape S5 forbids and the reason this figure lives here at all.
	t.Run("job_files caches failed_bytes beside its authority", func(t *testing.T) {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('job_files') WHERE name = 'failed_bytes'`,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Error("job_files.failed_bytes is missing — a non-resident job cannot report " +
				"failed bytes without it, and failed_articles records only WHICH articles " +
				"failed, never how many bytes they were")
		}
	})

	// job_files.bytes_downloaded is the sibling cache, and it is here for a
	// different reason than failed_bytes: the figure IS derivable from the
	// durability record, but only in decoded bytes. This one counts ENCODED
	// bytes, because it is subtracted from job_files.bytes to get a job's
	// remaining figure and that column is the encoded NZB total. Deriving it
	// from the record's lengths instead made every non-resident job overstate
	// its remaining bytes by the encoding overhead (#365).
	//
	// Asserted here rather than only in the queue package because the column's
	// existence is the decision — a queue-level test would go green again the
	// moment someone re-derived the figure from the wrong table.
	t.Run("job_files caches bytes_downloaded in encoded bytes", func(t *testing.T) {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('job_files') WHERE name = 'bytes_downloaded'`,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Error("job_files.bytes_downloaded is missing — a non-resident job has no " +
				"manifest to sum encoded article bytes from, and the durability record's " +
				"lengths are the decoded quantity, not this one")
		}
	})

	// The two-record store 002 replaced and 003 dropped. Asserted absent
	// rather than merely unused: while either table exists, a change can
	// reintroduce a writer for it, and the whole point of the durable-runs
	// design is that one download is described once.
	t.Run("the two-record tables are gone", func(t *testing.T) {
		for _, table := range []string{"article_facts", "file_extents"} {
			var n int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
			).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s is still present — one download must be described once, and "+
					"a surviving table is a place for a second writer to reappear", table)
			}
		}
	})

	// And the queue's own third copy. Article resolution is DERIVED from
	// durable_runs and failed_articles; a column here would be re-serialised
	// wholesale on every job update and free to disagree with both.
	t.Run("neither job_files table carries articles_done", func(t *testing.T) {
		for _, table := range []string{"job_files", "history_job_files"} {
			var n int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'articles_done'`, table,
			).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s.articles_done is back — it is a third copy of what "+
					"durable_runs and failed_articles already hold between them", table)
			}
		}
	})

	t.Run("durable_runs is keyed per file and offset", func(t *testing.T) {
		// Asserted through pragma_table_info's pk ordinals rather than a
		// "PRIMARY KEY" substring of the DDL. The substring is satisfied by a
		// key on any columns at all, including the wrong ones, while the
		// failure message names the columns — a check that cannot fail for the
		// reason it reports.
		//
		// Offset is the third column and that ordering is load-bearing:
		// SQLiteRunStore.queryBracketing scans the key range by offset within
		// a (job_id, file_idx) prefix, so a key that put offset anywhere else
		// would turn every commit into a full-file read.
		want := map[string]int{"job_id": 1, "file_idx": 2, "offset": 3}
		got := primaryKeyOrdinals(t, db, "durable_runs")
		if !maps.Equal(got, want) {
			t.Errorf("durable_runs primary key = %v, want %v", got, want)
		}
	})

	t.Run("failed_articles is keyed for an idempotent record", func(t *testing.T) {
		// RecordFailedArticles uses INSERT OR IGNORE, which is idempotent only
		// against a key on exactly this pair.
		want := map[string]int{"job_id": 1, "art_idx": 2}
		got := primaryKeyOrdinals(t, db, "failed_articles")
		if !maps.Equal(got, want) {
			t.Errorf("failed_articles primary key = %v, want %v — INSERT OR IGNORE "+
				"is only idempotent against a key on (job_id, art_idx)", got, want)
		}
	})

	t.Run("durable_runs carries what the three consumers need", func(t *testing.T) {
		// One column per question the record answers: the article range is the
		// resume set's complement, offset+length give the truncate bound and
		// the overlap check, and crc32 IS the whole-file CRC when a file
		// collapses to one row.
		for _, col := range []string{"first_art_idx", "last_art_idx", "offset", "length", "crc32"} {
			var n int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('durable_runs') WHERE name = ?`, col,
			).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("durable_runs missing column %q", col)
			}
		}
	})

	t.Run("no stray migration files", func(t *testing.T) {
		// This guarded a single 001_initial.sql until 002_durable_runs.sql
		// added the replacement record, 003_drop_legacy_durability.sql
		// dropped what it replaced, and 004_par2_release_reason.sql made the
		// par2 verdict's reason durable. It still exists to catch an
		// accidental extra file, just against a longer list.
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
		want := []string{
			"001_initial.sql", "002_durable_runs.sql",
			"003_drop_legacy_durability.sql", "004_par2_release_reason.sql",
		}
		if !slices.Equal(sqls, want) {
			t.Errorf("migrations = %v, want exactly %v", sqls, want)
		}
	})
}

// TestMigrations_GoldenSchema compares the schema the migration actually
// builds against a checked-in golden file.
//
// The subtests above pin the handful of properties this task's design turns
// on. This pins everything else — every table and index's stored DDL, and
// every foreign key — so that the constraints nothing else asserts cannot be
// dropped silently: WITHOUT ROWID on the two new tables, article_facts's
// covering index, job_files's UNIQUE(job_id, file_index) and its CASCADE to
// jobs, the fetch_policy CHECK, the AUTOINCREMENT on job_files.id, and the
// fact that history, jobs, and queue_meta came through the collapse of
// migrations 001-011 unchanged. sqlite_sequence appears in the dump and is
// deliberately not filtered out: SQLite creates it only for an AUTOINCREMENT
// column, so its presence is the assertion that job_files.id still has one.
//
// It matters more here than it usually would. The schema is one file now, so
// there is no chain of ALTERs recording what each column was for, and a
// constraint deleted from 001_initial.sql leaves no trace anywhere else.
//
// Whitespace is normalised because SQLite stores the DDL as written; the
// golden file is therefore stable against reformatting but not against a
// changed constraint. Regenerate it deliberately, with
// `go test ./internal/history/ -run TestMigrations_GoldenSchema -update`, and
// read the diff — an unexplained line in it is the bug this test exists for.
func TestMigrations_GoldenSchema(t *testing.T) {
	db := openMigratedTestDB(t)

	var b strings.Builder
	rows, err := db.Query(`
SELECT type, name, COALESCE(sql, '')
FROM sqlite_master
WHERE name NOT LIKE 'goose_%' AND tbl_name NOT LIKE 'goose_%'
ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	type object struct{ typ, name, sql string }
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.typ, &o.name, &o.sql); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	// Strip SQL line comments before collapsing whitespace. SQLite stores the
	// CREATE statement verbatim, comments included, and 001_initial.sql carries
	// most of its rationale inline — leaving them in would make this a
	// change-detector that fails on every comment edit while saying nothing
	// about the schema. The comments are reviewed as prose in the migration;
	// this file is for the constraints.
	lineComment := regexp.MustCompile(`(?m)--[^\n]*$`)
	ws := regexp.MustCompile(`\s+`)
	for _, o := range objects {
		// An implicit index (a UNIQUE or PRIMARY KEY constraint's own index)
		// has no DDL of its own. Recording the name alone still pins it:
		// dropping the constraint removes the row entirely.
		stmt := "(implicit)"
		if o.sql != "" {
			stmt = strings.TrimSpace(ws.ReplaceAllString(lineComment.ReplaceAllString(o.sql, ""), " "))
		}
		fmt.Fprintf(&b, "%s %s: %s\n", o.typ, o.name, stmt)

		if o.typ != "table" {
			continue
		}
		fks, err := db.Query(
			`SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?) ORDER BY id`, o.name)
		if err != nil {
			t.Fatal(err)
		}
		for fks.Next() {
			var target, from, to, onDelete string
			if err := fks.Scan(&target, &from, &to, &onDelete); err != nil {
				fks.Close()
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "  FK %s.%s -> %s.%s ON DELETE %s\n", o.name, from, target, to, onDelete)
		}
		if err := fks.Err(); err != nil {
			fks.Close()
			t.Fatal(err)
		}
		fks.Close()
	}
	got := b.String()

	golden := filepath.Join("testdata", "schema.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("schema differs from %s.\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}
