package durability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrInvalidExtent reports a FileExtent that cannot describe a real file.
var ErrInvalidExtent = errors.New("durability: invalid file extent")

// SQLiteExtentStore persists the Class B cache.
//
// Unlike the fact log, this store REPLACEs: a FileExtent is a cache entry
// describing the file as of the last barrier, so a newer commit supersedes an
// older one wholesale. The immutability rule that governs article_facts does
// not apply here, and must not be copied across.
type SQLiteExtentStore struct{ db *sql.DB }

// NewSQLiteExtentStore wraps db as an ExtentStore. The caller owns db's
// lifecycle.
func NewSQLiteExtentStore(db *sql.DB) *SQLiteExtentStore { return &SQLiteExtentStore{db: db} }

// Commit writes every extent in one transaction. Atomicity is the guarantee
// R7 depends on: a barrier that fails partway must leave the previously
// committed cache wholly intact rather than a mix of old and new.
func (s *SQLiteExtentStore) Commit(ctx context.Context, jobID string, exts []FileExtent) error {
	if len(exts) == 0 {
		return nil
	}
	for _, e := range exts {
		if e.VerifiedTo < 0 || e.Size < 0 || e.BytesDurable < 0 || e.BytesFailed < 0 {
			return fmt.Errorf("%w: file %d has a negative figure", ErrInvalidExtent, e.FileIdx)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin extent commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO file_extents
			(job_id, file_idx, durable_bitmap, verified_to, prefix_crc,
			 has_prefix_crc, bytes_durable, bytes_failed, size, mod_time_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("durability: prepare extent commit: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range exts {
		hasCRC := 0
		if e.HasPrefixCRC {
			hasCRC = 1
		}
		if _, err := stmt.ExecContext(ctx, jobID, e.FileIdx, e.Durable.Bytes(), e.VerifiedTo,
			e.PrefixCRC, hasCRC, e.BytesDurable, e.BytesFailed, e.Size, e.ModTimeNs); err != nil {
			return fmt.Errorf("durability: commit extent file=%d: %w", e.FileIdx, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durability: commit extent tx job=%s: %w", jobID, err)
	}
	return nil
}

// Load reads a job's extents, ordered by file_idx. The caller supplies the
// article count per file via the manifest; because the bitmap's bit count is
// not stored, Load reconstructs each bitmap at its full byte width and the
// caller narrows or widens it against the real count.
func (s *SQLiteExtentStore) Load(ctx context.Context, jobID string) ([]FileExtent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_idx, durable_bitmap, verified_to, prefix_crc,
		       has_prefix_crc, bytes_durable, bytes_failed, size, mod_time_ns
		  FROM file_extents WHERE job_id = ? ORDER BY file_idx`, jobID)
	if err != nil {
		return nil, fmt.Errorf("durability: query extents job=%s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FileExtent
	for rows.Next() {
		var e FileExtent
		var raw []byte
		var hasCRC int
		if err := rows.Scan(&e.FileIdx, &raw, &e.VerifiedTo, &e.PrefixCRC,
			&hasCRC, &e.BytesDurable, &e.BytesFailed, &e.Size, &e.ModTimeNs); err != nil {
			return nil, fmt.Errorf("durability: scan extent job=%s: %w", jobID, err)
		}
		e.HasPrefixCRC = hasCRC != 0
		bm, err := BitmapFromBytes(raw, len(raw)*8)
		if err != nil {
			return nil, fmt.Errorf("durability: extent job=%s file=%d bitmap: %w", jobID, e.FileIdx, err)
		}
		e.Durable = bm
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteJob removes every extent for a job that has left the queue.
func (s *SQLiteExtentStore) DeleteJob(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM file_extents WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("durability: delete extents for %s: %w", jobID, err)
	}
	return nil
}
