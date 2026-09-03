package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/job"
)

// ErrNoRow is returned when a checkpoint names a file that has no job_files
// row to update.
//
// It is an error rather than an insert, and rather than silence, because the
// two things it could mean are both defects and neither is this package's to
// repair. A row is written when a job is admitted, from the manifest — subject,
// date and bytes are NOT NULL and none of them is in a Checkpoint, so this
// package could not insert a correct row if it wanted to. And a missing row
// means the checkpoint went nowhere: the next hydration reads a Complete flag
// and a CRC from whatever is on disk, which is the direction that finalizes a
// file over bytes that are not there. Reporting is the only honest option.
var ErrNoRow = errors.New("checkpoint/store: no job_files row")

// Store persists job progress into the job_files table.
type Store struct {
	db *sql.DB
}

var _ checkpoint.Store = (*Store)(nil)

// New returns a Store over db. The caller owns db and its lifetime; Store
// neither opens nor closes it.
func New(db *sql.DB) *Store { return &Store{db: db} }

// updateFile is the per-file statement, in one place so that the column list
// and the argument order cannot drift apart. Every value is an integer or a
// string, so a transposition would not fail to compile — it would silently
// write a filename into a byte column's neighbour.
const updateFile = `UPDATE job_files SET
  complete = ?, fetch_policy = ?, filename = ?, assembled_crc32 = ?,
  failed_bytes = ?, bytes_downloaded = ?
WHERE job_id = ? AND file_index = ?`

// SaveBatch writes every checkpoint's per-file progress in ONE transaction:
// either all of them land or none does.
//
// The batch is atomic rather than best-effort per job because a partial batch
// is indistinguishable, on the next read, from a complete one — the rows carry
// no generation and nothing downstream can tell which jobs in a flush were
// written. Checkpointer.Flush already re-merges a failed batch's jobs back into
// its dirty set, so a rollback costs a retry rather than a lost transition.
//
// A checkpoint whose Progress is nil is skipped. That is a job with no content
// attached yet, not a job with empty progress: there is nothing to write and
// nothing to report.
//
// article_count is deliberately absent from the statement even though
// internal/queue's equivalent wrote it. That column is the file's article
// count, which lives in the manifest, and a Checkpoint carries progress
// without one — writing it from anything else would be inventing the value.
// It is written when the row is inserted and does not change afterwards.
func (s *Store) SaveBatch(ctx context.Context, cps []job.Checkpoint) error {
	if len(cps) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("checkpoint/store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, updateFile)
	if err != nil {
		return fmt.Errorf("checkpoint/store: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, cp := range cps {
		if err := saveOne(ctx, stmt, cp); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("checkpoint/store: commit: %w", err)
	}
	return nil
}

// saveOne writes one checkpoint's files through the prepared statement.
//
// It reads every figure through JobProgress's own accessors rather than
// reaching into the record, which is what keeps this package a persister
// rather than a second opinion about what a file's state is.
func saveOne(ctx context.Context, stmt *sql.Stmt, cp job.Checkpoint) error {
	p := cp.Progress
	if p == nil {
		return nil
	}
	for fi := range p.NumFiles() {
		res, err := stmt.ExecContext(ctx,
			p.FileComplete(fi), int(p.FileFetchPolicy(fi)), p.FileFilename(fi),
			p.FileAssembledCRC32(fi), p.FileFailedBytes(fi), p.FileBytesDownloaded(fi),
			cp.ID, fi,
		)
		if err != nil {
			return fmt.Errorf("checkpoint/store: save %s file %d: %w", cp.ID, fi, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("checkpoint/store: save %s file %d: rows affected: %w", cp.ID, fi, err)
		}
		if n == 0 {
			return fmt.Errorf("checkpoint/store: save %s file %d: %w", cp.ID, fi, ErrNoRow)
		}
	}
	return nil
}
