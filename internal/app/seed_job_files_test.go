package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// fetchAlwaysForAll is the fetch-policy accessor every seed test but the
// derivation-specific one below uses: every file is FetchAlways, matching
// the pre-#329 hardcoded seed value and keeping those tests' assertions
// about shape, idempotency, atomicity and cancellation unrelated to which
// policy is written.
func fetchAlwaysForAll(int) job.FetchPolicy { return job.FetchAlways }

// openHistoryTestDB returns a migrated history database.
func openHistoryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return history.NewRepository(db).DB()
}

// countSeeded returns how many job_files rows exist for one job.
func countSeeded(t *testing.T, db *sql.DB, jobID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, jobID,
	).Scan(&n); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	return n
}

// TestSeedJobFiles_OneRowPerFile pins the shape of the seed: exactly one row
// per file, at the file's own index, holding the derived fetch_policy and the
// empty results the recorder later fills in for everything else.
//
// The indices are asserted individually rather than only counted because a seed
// that wrote N rows at the wrong indices would satisfy a count and still leave
// every recorder UPDATE matching nothing — the silent failure the seed exists
// to prevent.
//
// fetch_policy is asserted against the derived value rather than against zero
// (#329): the seed authors it outright, so a non-zero policy here is the
// correct outcome for a recovery volume, not "results already set" — the
// pre-#329 shape this test used to pin.
func TestSeedJobFiles_OneRowPerFile(t *testing.T) {
	t.Parallel()
	db := openHistoryTestDB(t)

	derived := map[int]job.FetchPolicy{2: job.FetchIfNeeded}
	fetch := func(i int) job.FetchPolicy {
		if p, ok := derived[i]; ok {
			return p
		}
		return job.FetchAlways
	}

	if err := seedJobFiles(t.Context(), durability.NewStore(db), blankJob(t, "job-a", 4, fetch)); err != nil {
		t.Fatalf("seedJobFiles: %v", err)
	}

	rows, err := db.Query(
		`SELECT file_index, complete, fetch_policy, filename
		   FROM job_files WHERE job_id = ? ORDER BY file_index`, "job-a")
	if err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []int
	for rows.Next() {
		var fi, complete, policy int
		var filename string
		if err := rows.Scan(&fi, &complete, &policy, &filename); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if complete != 0 || filename != "" {
			t.Errorf("file %d seeded with placeholder results already set: complete=%d filename=%q",
				fi, complete, filename)
		}
		if want := int(fetch(fi)); policy != want {
			t.Errorf("file %d fetch_policy = %d, want %d (the derived policy)", fi, policy, want)
		}
		got = append(got, fi)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	want := []int{0, 1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("seeded indices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seeded indices = %v, want %v", got, want)
		}
	}
}

// TestSeedJobFiles_IsIdempotent pins the ON CONFLICT DO NOTHING clause. A job
// re-added under the same ID must not fail, and must not disturb results the
// recorder has already written — DO NOTHING rather than an upsert, because an
// upsert would reset a completed file's filename and complete flag.
func TestSeedJobFiles_IsIdempotent(t *testing.T) {
	t.Parallel()
	db := openHistoryTestDB(t)

	if err := seedJobFiles(t.Context(), durability.NewStore(db), blankJob(t, "job-b", 3, fetchAlwaysForAll)); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE job_files SET complete = 1, filename = 'a.rar' WHERE job_id = ? AND file_index = 1`,
		"job-b",
	); err != nil {
		t.Fatalf("simulate recorder write: %v", err)
	}
	if err := seedJobFiles(t.Context(), durability.NewStore(db), blankJob(t, "job-b", 3, fetchAlwaysForAll)); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	if n := countSeeded(t, db, "job-b"); n != 3 {
		t.Errorf("job_files rows after re-seed = %d, want 3", n)
	}
	var complete int
	var filename string
	if err := db.QueryRow(
		`SELECT complete, filename FROM job_files WHERE job_id = ? AND file_index = 1`, "job-b",
	).Scan(&complete, &filename); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if complete != 1 || filename != "a.rar" {
		t.Errorf("re-seed clobbered recorded results: complete=%d filename=%q, want 1 and \"a.rar\"",
			complete, filename)
	}
}

// TestSeedJobFiles_IsAllOrNothing pins the transaction. A failure partway
// through must leave NO rows, not a prefix of them.
//
// The prefix is the dangerous outcome rather than the empty result: by the time
// the seed runs the job is already registered with the dispatcher and about to
// download, and SaveBatch's UPDATE matching no row is not an error, so the
// unseeded tail would persist no metadata at all and hydrate with defaults —
// with nothing logged anywhere.
//
// The fault is injected with a trigger rather than a production seam: the seed
// takes a *sql.DB and has no injection point, and a constraint that fires
// inside SQLite is a truer stand-in for the real failures here (a disk error, a
// busy timeout) than a Go-side hook would be.
func TestSeedJobFiles_IsAllOrNothing(t *testing.T) {
	t.Parallel()
	db := openHistoryTestDB(t)

	if _, err := db.Exec(`
CREATE TRIGGER fail_on_index_three BEFORE INSERT ON job_files
WHEN NEW.file_index = 3
BEGIN SELECT RAISE(ABORT, 'injected fault'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err := seedJobFiles(t.Context(), durability.NewStore(db), blankJob(t, "job-c", 6, fetchAlwaysForAll))
	if err == nil {
		t.Fatal("seedJobFiles returned nil, want the injected fault reported")
	}
	if n := countSeeded(t, db, "job-c"); n != 0 {
		t.Errorf("job_files rows after a failed seed = %d, want 0 — files 0..2 committed "+
			"before the fault, so the tail of this job would silently persist no metadata", n)
	}
}

// TestSeedJobFiles_ZeroFilesCommits pins that a manifest with no files is not
// an error. The loop body never runs, so this reaches Commit with an empty
// transaction, which is the one path the other three tests never take.
func TestSeedJobFiles_ZeroFilesCommits(t *testing.T) {
	t.Parallel()
	db := openHistoryTestDB(t)

	if err := seedJobFiles(t.Context(), durability.NewStore(db), blankJob(t, "job-d", 0, fetchAlwaysForAll)); err != nil {
		t.Fatalf("seedJobFiles with no files: %v", err)
	}
	if n := countSeeded(t, db, "job-d"); n != 0 {
		t.Errorf("job_files rows = %d, want 0", n)
	}
}

// TestSeedJobFiles_CancelledContextSeedsNothing pins the BeginTx error path.
func TestSeedJobFiles_CancelledContextSeedsNothing(t *testing.T) {
	t.Parallel()
	db := openHistoryTestDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := seedJobFiles(ctx, durability.NewStore(db), blankJob(t, "job-e", 3, fetchAlwaysForAll)); err == nil {
		t.Fatal("seedJobFiles returned nil for a cancelled context, want an error")
	}
	if n := countSeeded(t, db, "job-e"); n != 0 {
		t.Errorf("job_files rows = %d, want 0", n)
	}
}

// blankJob returns a job with n structure-less files whose fetch policies are
// fetch's, for tests of the seed's row count and fetch policies rather than of
// the structure it copies.
func blankJob(t *testing.T, id string, n int, fetch func(int) job.FetchPolicy) *job.Job {
	t.Helper()
	j := job.New(id, id, job.Policy{})
	if err := j.AttachContent(job.NewManifest(make([]job.JobFile, n))); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	for i := range n {
		if err := j.SetFileFetchPolicy(i, fetch(i)); err != nil {
			t.Fatalf("SetFileFetchPolicy(%d): %v", i, err)
		}
	}
	return j
}

// TestSeedJobFiles_RefusesAJobWithoutFiles pins that a job with no file table
// is an error rather than a zero-row seed.
func TestSeedJobFiles_RefusesAJobWithoutFiles(t *testing.T) {
	t.Parallel()
	db := openHistoryTestDB(t)

	err := seedJobFiles(t.Context(), durability.NewStore(db), job.New("job-f", "job-f", job.Policy{}))
	if err == nil {
		t.Fatal("seedJobFiles returned nil for a job with no file table, want an error")
	}
	if n := countSeeded(t, db, "job-f"); n != 0 {
		t.Errorf("job_files rows = %d, want 0", n)
	}
}

// TestSeedJobFiles_WritesTheStructure pins that AddJob seeds each file's
// subject, bytes, article count and fetch policy from the job's file table, in
// a job with an on-demand par2 volume so a FetchIfNeeded row is exercised.
func TestSeedJobFiles_WritesTheStructure(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a := env.newApp(t)
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="p" date="1" subject="data.part01.rar">
  <groups><group>alt.binaries.test</group></groups>
  <segments><segment bytes="100" number="1">d1@t</segment><segment bytes="150" number="2">d2@t</segment></segments>
 </file>
 <file poster="p" date="1" subject="data.vol01+02.par2">
  <groups><group>alt.binaries.test</group></groups>
  <segments><segment bytes="80" number="1">p1@t</segment></segments>
 </file>
</nzb>`)
	parsed, err := nzb.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	a.config.With(func(c *config.Config) { c.Downloads.OnDemandPar2 = true })
	j, hdr, err := BuildIngestJob(a.config, parsed, "data.nzb", types.FetchOptions{NzbName: "data"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := a.AddJob(t.Context(), j, hdr, raw, true); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	rows, err := a.st.FileRows(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("FileRows: %v", err)
	}
	specs := j.Files().Specs()
	if len(rows) != len(specs) || len(rows) != 2 {
		t.Fatalf("rows = %d, specs = %d, want 2 each", len(rows), len(specs))
	}
	sawIfNeeded := false
	for i, r := range rows {
		if r.FileIndex != i || r.Subject != specs[i].Subject || r.Bytes != specs[i].Bytes || r.ArticleCount != specs[i].ArticleCount {
			t.Errorf("row %d = %+v, want structure %+v", i, r, specs[i])
		}
		if want := uint8(j.FileFetchPolicy(i)); r.FetchPolicy != want {
			t.Errorf("row %d FetchPolicy = %d, want %d", i, r.FetchPolicy, want)
		}
		sawIfNeeded = sawIfNeeded || r.FetchPolicy == uint8(job.FetchIfNeeded)
	}
	if !sawIfNeeded {
		t.Error("no FetchIfNeeded row: the fixture does not exercise the derived policy")
	}
	if rows[0].ArticleCount != 2 || rows[0].Bytes != 250 {
		t.Errorf("row 0 = %+v, want ArticleCount 2, Bytes 250", rows[0])
	}
}
