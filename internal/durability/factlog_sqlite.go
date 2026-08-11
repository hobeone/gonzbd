package durability

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLiteFactLog is the append-only Class A store.
type SQLiteFactLog struct{ db *sql.DB }

// NewSQLiteFactLog wraps db as a FactLog. The caller owns db's lifecycle.
func NewSQLiteFactLog(db *sql.DB) *SQLiteFactLog { return &SQLiteFactLog{db: db} }

var _ FactLog = (*SQLiteFactLog)(nil)

// Append inserts facts, ignoring any whose (job_id, art_idx) is already
// present. INSERT OR IGNORE rather than UPSERT is the point: a Class A fact
// is immutable (R1), so a second delivery of the same article must leave the
// original record untouched. The file's bytes were written against the
// original offset, and a later report claiming a different one — whether from
// a buggy path or a hostile server — must not be allowed to redescribe them.
func (s *SQLiteFactLog) Append(ctx context.Context, jobID string, facts []ArticleFact) error {
	if len(facts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin fact append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO article_facts
			(job_id, art_idx, file_idx, offset, length, crc32, has_crc)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("durability: prepare fact append: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, f := range facts {
		hasCRC := 0
		if f.HasCRC {
			hasCRC = 1
		}
		if _, err := stmt.ExecContext(ctx, jobID, f.ArtIdx, f.FileIdx, f.Offset, f.Length, f.CRC32, hasCRC); err != nil {
			return fmt.Errorf("durability: append fact job=%s art=%d: %w", jobID, f.ArtIdx, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durability: commit fact append job=%s: %w", jobID, err)
	}
	return nil
}

// ForFile returns every recorded fact for one file, ordered by Offset.
func (s *SQLiteFactLog) ForFile(ctx context.Context, jobID string, fileIdx int32) ([]ArticleFact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT art_idx, file_idx, offset, length, crc32, has_crc
		  FROM article_facts
		 WHERE job_id = ? AND file_idx = ?
		 ORDER BY offset`, jobID, fileIdx)
	if err != nil {
		return nil, fmt.Errorf("durability: query facts job=%s file=%d: %w", jobID, fileIdx, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ArticleFact
	for rows.Next() {
		var f ArticleFact
		var hasCRC int
		if err := rows.Scan(&f.ArtIdx, &f.FileIdx, &f.Offset, &f.Length, &f.CRC32, &hasCRC); err != nil {
			return nil, fmt.Errorf("durability: scan fact job=%s file=%d: %w", jobID, fileIdx, err)
		}
		f.HasCRC = hasCRC != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteJob removes every fact for a job that has left the queue.
func (s *SQLiteFactLog) DeleteJob(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM article_facts WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("durability: delete facts for %s: %w", jobID, err)
	}
	return nil
}
