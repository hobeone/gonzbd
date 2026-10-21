package durability

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// ErrIncomplete marks a read that failed partway: the rows returned with it are
// the ones read before the failure, and a caller may use them. A read error
// that does not wrap it returned nothing usable.
var ErrIncomplete = errors.New("durability: read incomplete")

// FileRow is one job_files row: a file's NZB-derived structure (subject, bytes,
// article count), its fetch policy, and its persisted outcome (complete,
// filename).
//
// FetchPolicy is job.FetchPolicy's underlying type. This package cannot name
// job.FetchPolicy, because internal/job imports internal/durability; the
// adapter in internal/app converts at the boundary.
type FileRow struct {
	FileIndex    int
	Subject      string
	Bytes        int64
	ArticleCount int
	Complete     bool
	FetchPolicy  uint8
	Filename     string
}

// AdmitFile is what Admit writes for one file: its NZB-derived structure and
// its fetch policy. Position in the slice is the file index.
type AdmitFile struct {
	Subject      string
	Bytes        int64
	ArticleCount int
	FetchPolicy  uint8
}

// Admit seeds one job_files row per file, files[i] being file i.
//
// It is a precondition for the recorder's writes (ApplyRecord), not an
// optimisation: they update job_files rows by (job_id, file_index) and insert
// written_articles only for a job that has a job_files row, and an UPDATE
// matching no row is not an error, so a file with no seed row silently
// persists no progress at all and hydrates with defaults. That is why the
// whole seed is one transaction — partway through, the job may already be
// registered and about to download, and a per-row autocommit would leave the
// tail of the file list in exactly that silent state. It is also where the
// cost is: each autocommit is its own WAL commit, so a thousand-file NZB paid
// a thousand of them at submission.
//
// subject, bytes, article_count and fetch_policy are authored here; each is
// fully determined before this is called. complete and filename are RESULTS,
// seeded empty for the recorder to fill in. A row that already exists is left
// alone whole, so a retry re-seeding a job keeps the progress it retained.
// That includes the structure columns: a retry keeps the subject, bytes and
// article_count the first parse wrote (`git grep -n '^ON CONFLICT' --
// internal/durability/progress.go` finds 1 line). Within one build the
// re-parse of the backed-up NZB yields the same structure; a parser change
// across builds is the case Standing Design Rule 1 waives, and stage 2
// revisits it.
func (s *Store) Admit(ctx context.Context, jobID string, files []AdmitFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin job_files seed %s: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO job_files
  (job_id, file_index, subject, bytes, article_count, complete, fetch_policy, filename)
VALUES (?, ?, ?, ?, ?, 0, ?, '')
ON CONFLICT(job_id, file_index) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("durability: prepare job_files seed %s: %w", jobID, err)
	}
	defer func() { _ = stmt.Close() }()

	for i, f := range files {
		if _, err := stmt.ExecContext(ctx, jobID, i, f.Subject, f.Bytes, f.ArticleCount, int(f.FetchPolicy)); err != nil {
			return fmt.Errorf("durability: insert job_file %s index %d: %w", jobID, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durability: commit job_files seed %s: %w", jobID, err)
	}
	return nil
}

// FileRows returns a job's job_files rows, ordered by file index.
//
// A row that fails to scan is skipped and the rest are still returned, with
// the error wrapping ErrIncomplete. The partial result is returned for a
// caller that wants to inspect it; none does today — every non-test call
// returns on the error without reading the rows
// (`git grep -n '[.]FileRows(ctx' -- '*.go' ':!*_test.go'` returns 4 lines).
func (s *Store) FileRows(ctx context.Context, jobID string) ([]FileRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_index, subject, bytes, article_count, filename, complete, fetch_policy FROM job_files WHERE job_id = ? ORDER BY file_index`, jobID)
	if err != nil {
		return nil, fmt.Errorf("durability: load job_files %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FileRow
	var errs []error
	for rows.Next() {
		var r FileRow
		var complete, fetch int
		if err := rows.Scan(&r.FileIndex, &r.Subject, &r.Bytes, &r.ArticleCount, &r.Filename, &complete, &fetch); err != nil {
			errs = append(errs, fmt.Errorf("durability: scan job_files %s: %w: %w", jobID, ErrIncomplete, err))
			continue
		}
		r.Complete = complete != 0
		r.FetchPolicy = uint8(fetch) //nolint:gosec // G115: fetch_policy is CHECKed to 0-2 by the schema
		out = append(out, r)
	}
	// rows.Next() returns false for "no more rows" AND for a mid-iteration
	// fault, so without this a dropped connection reads as a complete result
	// set.
	if err := rows.Err(); err != nil {
		errs = append(errs, fmt.Errorf("durability: iterate job_files %s: %w: %w", jobID, ErrIncomplete, err))
	}
	return out, errors.Join(errs...)
}

// SeedRow is one job_files row plus the number of written_articles rows the
// record holds for that file. Read by the restore pass,
// Application.restoreProgress, which runs once per Start
// (`git grep -n 'durable[.]SeedRows' -- '*.go' ':!*_test.go'` returns 1 line,
// its call); FileRows is the per-job read and carries no count.
type SeedRow struct {
	FileRow
	Recorded int
}

// SeedRows returns every job's readable rows, keyed by job ID, ordered by file
// index within a job. A job with no job_files rows is absent from the map.
//
// A row whose columns do not read as their types (SQLite is dynamically typed,
// so a damaged or hand-edited row can hold text where a count belongs) costs
// its job and no other: every row of that job is dropped from the map, and the
// job is named in unreadable with the file index and column of its first bad
// row. err is only for a query or iteration that failed as a whole, and then
// nothing returned is usable. written_articles is PRIMARY KEY (job_id,
// file_idx, art_idx), so the GROUP BY is a key-prefix scan.
func (s *Store) SeedRows(ctx context.Context) (rows map[string][]SeedRow, unreadable map[string]error, err error) {
	res, err := s.db.QueryContext(ctx, `
SELECT f.job_id, f.file_index, f.subject, f.bytes, f.article_count, f.filename, f.complete, f.fetch_policy, COALESCE(w.n, 0)
FROM job_files f
LEFT JOIN (SELECT job_id, file_idx, COUNT(*) AS n FROM written_articles GROUP BY job_id, file_idx) w
  ON w.job_id = f.job_id AND w.file_idx = f.file_index
ORDER BY f.job_id, f.file_index`)
	if err != nil {
		return nil, nil, fmt.Errorf("durability: load seed rows: %w", err)
	}
	defer func() { _ = res.Close() }()

	rows = make(map[string][]SeedRow)
	unreadable = make(map[string]error)
	for res.Next() {
		var jobID string
		var cols [8]any
		if err := res.Scan(&jobID, &cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6], &cols[7]); err != nil {
			// The key itself is unreadable: no job can be named.
			return nil, nil, fmt.Errorf("durability: scan seed row key: %w", err)
		}
		if _, bad := unreadable[jobID]; bad {
			continue
		}
		r, err := seedRowOf(cols)
		if err != nil {
			unreadable[jobID] = err
			delete(rows, jobID)
			continue
		}
		rows[jobID] = append(rows[jobID], r)
	}
	if err := res.Err(); err != nil {
		return nil, nil, fmt.Errorf("durability: iterate seed rows: %w", err)
	}
	return rows, unreadable, nil
}

// seedRowOf converts one SeedRows result row's columns after job_id, in select
// order, to a SeedRow, or names the file index and column that does not read.
func seedRowOf(c [8]any) (SeedRow, error) {
	idx, ok := c[0].(int64)
	if !ok {
		return SeedRow{}, fmt.Errorf("job_files row has a non-integer file_index (%T)", c[0])
	}
	bad := func(col string, v any) (SeedRow, error) {
		return SeedRow{}, fmt.Errorf("job_files row at file index %d: column %s holds %T %v, not a value of its type", idx, col, v, v)
	}
	ints := [...]struct {
		name string
		v    any
		max  int64
	}{
		{"bytes", c[2], math.MaxInt64},
		{"article_count", c[3], math.MaxInt32},
		{"complete", c[5], math.MaxInt64},
		{"fetch_policy", c[6], 2},
		{"recorded", c[7], math.MaxInt32},
	}
	var n [len(ints)]int64
	for i, col := range ints {
		v, ok := col.v.(int64)
		if !ok || v < 0 || v > col.max {
			return bad(col.name, col.v)
		}
		n[i] = v
	}
	subject, ok := c[1].(string)
	if !ok {
		return bad("subject", c[1])
	}
	filename, ok := c[4].(string)
	if !ok {
		return bad("filename", c[4])
	}
	return SeedRow{
		FileIndex:    int(idx),
		Subject:      subject,
		Bytes:        n[0],
		ArticleCount: int(n[1]),
		Complete:     n[2] != 0,
		FetchPolicy:  uint8(n[3]), //nolint:gosec // G115: range-checked to 0-2 above
		Filename:     filename,
		Recorded:     int(n[4]),
	}, nil
}
