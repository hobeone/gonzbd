package durability

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// WrittenRow is one written_articles row: an article whose decoded bytes were
// handed to pwrite at (Offset, Length), with the decoder's CRC of those bytes.
// It is not a durability claim; see the table's comment in 001_initial.sql.
type WrittenRow struct {
	FileIdx int
	ArtIdx  int32
	Offset  int64
	Length  int64
	CRC32   uint32
}

// HasValidShape reports whether the row's range is one a write can have
// produced: a non-negative offset and a non-negative length. A zero-length
// article is written with n == 0 (the assembler reports it through
// OnArticleWritten) and its row verifies zero bytes against CRC 0, so it is
// valid; it claims no byte range. The live door (job.Job.MarkArticleWritten)
// and a restart's (job.placeRows, app.readBackFile) each judge a row by this
// method, so a row the live door accepts is not dropped after a restart.
func (r WrittenRow) HasValidShape() bool { return r.Offset >= 0 && r.Length >= 0 }

// CompareWrittenRows orders rows by offset, then article index.
func CompareWrittenRows(a, b WrittenRow) int {
	return cmp.Or(cmp.Compare(a.Offset, b.Offset), cmp.Compare(a.ArtIdx, b.ArtIdx))
}

// FileState is the job_files columns the flusher updates.
type FileState struct {
	FileIdx     int
	Complete    bool
	Filename    string
	FetchPolicy uint8
}

// FileVerdict is a verification or untrust result for one file.
type FileVerdict struct {
	FileIdx       int
	DeleteAll     bool    // every row of the file is deleted
	DeleteArtIdxs []int32 // rows to delete when !DeleteAll
	ClearComplete bool
	SetComplete   bool // set after a successful finish-by-path
}

// RecordBatch is one job's share of a flusher transaction.
type RecordBatch struct {
	JobID    string
	Rows     []WrittenRow
	Files    []FileState
	Verdicts []FileVerdict
}

// errVerdictBothWays is returned for a FileVerdict that sets and clears
// complete at once: the two cannot both be applied.
var errVerdictBothWays = errors.New("durability: verdict both sets and clears complete")

// ApplyRecord applies every batch in one transaction. Within a batch the order
// is: verdict deletes, row upserts, file states, verdict complete changes.
//
// A row is written when the job still has a job_files row, so a flush that
// races a job's reclaim cannot resurrect rows nothing reaches.
func (s *Store) ApplyRecord(ctx context.Context, batches []RecordBatch) error {
	if len(batches) == 0 {
		return nil
	}
	return s.inTx(ctx, "apply record", func(tx *sql.Tx) error {
		for _, b := range batches {
			if err := applyBatch(ctx, tx, b); err != nil {
				return err
			}
		}
		return nil
	})
}

func applyBatch(ctx context.Context, tx *sql.Tx, b RecordBatch) error {
	for _, v := range b.Verdicts {
		if v.SetComplete && v.ClearComplete {
			return fmt.Errorf("%w: job %s file %d", errVerdictBothWays, b.JobID, v.FileIdx)
		}
		if err := deleteVerdict(ctx, tx, b.JobID, v); err != nil {
			return err
		}
	}
	if err := insertRows(ctx, tx, b); err != nil {
		return err
	}
	for _, f := range b.Files {
		if _, err := tx.ExecContext(ctx,
			`UPDATE job_files SET complete = ?, filename = ?, fetch_policy = ? WHERE job_id = ? AND file_index = ?`,
			boolInt(f.Complete), f.Filename, int(f.FetchPolicy), b.JobID, f.FileIdx); err != nil {
			return fmt.Errorf("durability: update job_files %s file %d: %w", b.JobID, f.FileIdx, err)
		}
	}
	for _, v := range b.Verdicts {
		if !v.SetComplete && !v.ClearComplete {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE job_files SET complete = ? WHERE job_id = ? AND file_index = ?`,
			boolInt(v.SetComplete), b.JobID, v.FileIdx); err != nil {
			return fmt.Errorf("durability: verdict complete %s file %d: %w", b.JobID, v.FileIdx, err)
		}
	}
	return nil
}

// insertRows writes a batch's rows when the job still has a job_files row,
// which it checks once for the batch and prepares the insert once for it.
func insertRows(ctx context.Context, tx *sql.Tx, b RecordBatch) error {
	if len(b.Rows) == 0 {
		return nil
	}
	var live bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM job_files WHERE job_id = ?)`, b.JobID).Scan(&live); err != nil {
		return fmt.Errorf("durability: check job_files %s: %w", b.JobID, err)
	}
	if !live {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("durability: prepare written insert %s: %w", b.JobID, err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range b.Rows {
		if _, err := stmt.ExecContext(ctx, b.JobID, r.FileIdx, r.ArtIdx, r.Offset, r.Length, r.CRC32); err != nil {
			return fmt.Errorf("durability: record written article %s file %d art %d: %w", b.JobID, r.FileIdx, r.ArtIdx, err)
		}
	}
	return nil
}

func deleteVerdict(ctx context.Context, tx *sql.Tx, jobID string, v FileVerdict) error {
	if v.DeleteAll {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM written_articles WHERE job_id = ? AND file_idx = ?`, jobID, v.FileIdx); err != nil {
			return fmt.Errorf("durability: delete written file %s file %d: %w", jobID, v.FileIdx, err)
		}
		return nil
	}
	for _, a := range v.DeleteArtIdxs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM written_articles WHERE job_id = ? AND file_idx = ? AND art_idx = ?`, jobID, v.FileIdx, a); err != nil {
			return fmt.Errorf("durability: delete written article %s file %d art %d: %w", jobID, v.FileIdx, a, err)
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// WrittenRows returns a job's written_articles rows, ordered by file then
// offset.
func (s *Store) WrittenRows(ctx context.Context, jobID string) ([]WrittenRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT file_idx, art_idx, offset, length, crc32 FROM written_articles
 WHERE job_id = ? ORDER BY file_idx, offset`, jobID)
	if err != nil {
		return nil, fmt.Errorf("durability: load written_articles %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []WrittenRow
	for rows.Next() {
		var r WrittenRow
		if err := rows.Scan(&r.FileIdx, &r.ArtIdx, &r.Offset, &r.Length, &r.CRC32); err != nil {
			return nil, fmt.Errorf("durability: scan written_articles %s: %w", jobID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("durability: iterate written_articles %s: %w", jobID, err)
	}
	return out, nil
}
