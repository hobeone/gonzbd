package durability

import (
	"context"
	"errors"
	"fmt"
)

// ErrIncomplete marks a read that failed partway: the rows returned with it are
// the ones read before the failure, and a caller may use them. A read error
// that does not wrap it returned nothing usable.
var ErrIncomplete = errors.New("durability: read incomplete")

// FileRow is one job_files row: a file's persisted download progress.
//
// FetchPolicy is job.FetchPolicy's underlying type. This package cannot name
// job.FetchPolicy, because internal/job imports internal/durability; the
// adapter in internal/app converts at the boundary.
type FileRow struct {
	FileIndex      int
	Complete       bool
	FetchPolicy    uint8
	Filename       string
	AssembledCRC32 uint32
}

// JobProgress is one job's checkpoint, as SaveProgress writes it.
type JobProgress struct {
	JobID          string
	Files          []FileRow
	FailedArticles []int
}

// Admit seeds one job_files row per file, fetch[i] being file i's policy.
//
// It is a precondition for SaveProgress, not an optimisation: SaveProgress
// UPDATEs by (job_id, file_index), and an UPDATE matching no row is not an
// error, so a file with no seed row silently persists no progress at all and
// hydrates with defaults. That is why the whole seed is one transaction —
// partway through, the job may already be registered and about to download,
// and a per-row autocommit would leave the tail of the file list in exactly
// that silent state. It is also where the cost is: each autocommit is its own
// WAL commit, so a thousand-file NZB paid a thousand of them at submission.
//
// Only fetch_policy is authored here. It is fully determined before this is
// called; complete, filename and assembled_crc32 are RESULTS, seeded empty for
// SaveProgress to fill in. A row that already exists is left alone, so a retry
// re-seeding a job keeps the progress it retained.
func (s *Store) Admit(ctx context.Context, jobID string, fetch []uint8) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin job_files seed %s: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO job_files
  (job_id, file_index, complete, fetch_policy, filename, assembled_crc32)
VALUES (?, ?, 0, ?, '', 0)
ON CONFLICT(job_id, file_index) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("durability: prepare job_files seed %s: %w", jobID, err)
	}
	defer func() { _ = stmt.Close() }()

	for i, f := range fetch {
		if _, err := stmt.ExecContext(ctx, jobID, i, int(f)); err != nil {
			return fmt.Errorf("durability: insert job_file %s index %d: %w", jobID, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durability: commit job_files seed %s: %w", jobID, err)
	}
	return nil
}

// SaveProgress writes a batch of checkpoints in one transaction: each file's
// row in job_files, and each failed article in failed_articles.
//
// A file's row is UPDATEd, never inserted — see Admit for why that makes the
// seed a precondition. Failed articles are INSERT OR IGNORE, so a mark is
// never removed here; only the discards below remove one.
func (s *Store) SaveProgress(ctx context.Context, batch []JobProgress) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin checkpoint: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmtFiles, err := tx.PrepareContext(ctx,
		`UPDATE job_files SET complete = ?, fetch_policy = ?, filename = ?, assembled_crc32 = ? WHERE job_id = ? AND file_index = ?`,
	)
	if err != nil {
		return fmt.Errorf("durability: prepare job_files: %w", err)
	}
	defer func() { _ = stmtFiles.Close() }()

	stmtFailed, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?1)`,
	)
	if err != nil {
		return fmt.Errorf("durability: prepare failed_articles: %w", err)
	}
	defer func() { _ = stmtFailed.Close() }()

	for _, jp := range batch {
		for _, f := range jp.Files {
			complete := 0
			if f.Complete {
				complete = 1
			}
			if _, err := stmtFiles.ExecContext(ctx,
				complete, int(f.FetchPolicy), f.Filename, f.AssembledCRC32,
				jp.JobID, f.FileIndex,
			); err != nil {
				return fmt.Errorf("durability: update job_file %s index %d: %w", jp.JobID, f.FileIndex, err)
			}
		}
		for _, artIdx := range jp.FailedArticles {
			if _, err := stmtFailed.ExecContext(ctx, jp.JobID, artIdx); err != nil {
				return fmt.Errorf("durability: insert failed_article %s index %d: %w", jp.JobID, artIdx, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durability: commit checkpoint: %w", err)
	}
	return nil
}

// FileRows returns a job's job_files rows, ordered by file index.
//
// A row that fails to scan is skipped and the rest are still returned, with
// the error wrapping ErrIncomplete: that file keeps whatever default the
// caller's fresh progress record gave it, which is better than dropping the
// whole job's file metadata for one bad row.
func (s *Store) FileRows(ctx context.Context, jobID string) ([]FileRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_index, filename, complete, assembled_crc32, fetch_policy FROM job_files WHERE job_id = ? ORDER BY file_index`, jobID)
	if err != nil {
		return nil, fmt.Errorf("durability: load job_files %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FileRow
	var errs []error
	for rows.Next() {
		var r FileRow
		var complete, fetch int
		if err := rows.Scan(&r.FileIndex, &r.Filename, &complete, &r.AssembledCRC32, &fetch); err != nil {
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

// FailedArticles returns the article indices marked failed for a job, in
// ascending order.
//
// It stops at the first row that fails to scan, and returns the indices read
// before it with an error wrapping ErrIncomplete.
func (s *Store) FailedArticles(ctx context.Context, jobID string) ([]int32, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT art_idx FROM failed_articles WHERE job_id = ? ORDER BY art_idx`, jobID)
	if err != nil {
		return nil, fmt.Errorf("durability: query failed_articles %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int32
	for rows.Next() {
		var idx int32
		if err := rows.Scan(&idx); err != nil {
			return out, fmt.Errorf("durability: scan failed_articles %s: %w: %w", jobID, ErrIncomplete, err)
		}
		out = append(out, idx)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("durability: iterate failed_articles %s: %w: %w", jobID, ErrIncomplete, err)
	}
	return out, nil
}
