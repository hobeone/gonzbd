package durability

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

// SQLiteRunStore persists the merged Class A + Class B durable_runs record.
//
// Unlike SQLiteFactLog's append-only INSERT OR IGNORE, this store REPLACEs:
// merging is read-modify-write by construction (§3.1 of the design doc), so
// a commit may delete and re-insert rows it owns. That is safe because a row
// is only ever written after a completed fsync — a later commit can only
// describe MORE durable bytes than an earlier one, never different ones for
// the same article.
type SQLiteRunStore struct{ db *sql.DB }

// NewSQLiteRunStore wraps db as a RunStore. The caller owns db's lifecycle.
func NewSQLiteRunStore(db *sql.DB) *SQLiteRunStore { return &SQLiteRunStore{db: db} }

var _ RunStore = (*SQLiteRunStore)(nil)

// Commit groups arts into runs and merges them into durable_runs, atomically
// and idempotently against redelivery.
//
// The order is fixed and load-bearing (design doc §6): SUBTRACT every
// incoming article whose ArtIdx a stored row already covers, THEN SORT what
// remains by Offset, THEN GROUP it into maximal runs, THEN MERGE those runs
// against each other and against the stored rows they abut. Grouping before
// subtracting can build a run that bridges a redelivered span into genuinely
// new work, which no stored row covers and no whole-run check can catch —
// see run.go's Commit doc for the worked example this order prevents.
func (s *SQLiteRunStore) Commit(ctx context.Context, jobID string, arts []DurableArticle) error {
	if len(arts) == 0 {
		return nil
	}

	byFile := make(map[int32][]DurableArticle)
	for _, a := range arts {
		byFile[a.FileIdx] = append(byFile[a.FileIdx], a)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin run commit job=%s: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Sorting the file indices makes the write order deterministic, which
	// costs nothing here and keeps this method's behaviour reproducible
	// under a test that inspects the transaction's statement sequence.
	fileIdxs := make([]int32, 0, len(byFile))
	for fi := range byFile {
		fileIdxs = append(fileIdxs, fi)
	}
	slices.Sort(fileIdxs)

	for _, fi := range fileIdxs {
		if err := s.commitFile(ctx, tx, jobID, fi, byFile[fi]); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durability: commit run tx job=%s: %w", jobID, err)
	}
	return nil
}

// commitFile applies Commit's algorithm to one file's incoming articles,
// within the caller's transaction.
func (s *SQLiteRunStore) commitFile(ctx context.Context, tx *sql.Tx, jobID string, fileIdx int32, arts []DurableArticle) error {
	sort.Slice(arts, func(i, j int) bool { return arts[i].Offset < arts[j].Offset })

	var maxEnd int64
	for _, a := range arts {
		if end := a.Offset + int64(a.Length); end > maxEnd {
			maxEnd = end
		}
	}

	// Range-scoped read: only stored rows starting at or before the
	// incoming articles' offset span can dedup or merge against them.
	// offset is the leading column after (job_id, file_idx) in the primary
	// key, so this is an index range scan, not a full-file read.
	stored, err := s.queryBracketing(ctx, tx, jobID, fileIdx, maxEnd)
	if err != nil {
		return err
	}

	// Subtract: drop every incoming article whose ArtIdx a stored row
	// already covers. Must happen before grouping — see Commit's doc.
	kept := make([]DurableArticle, 0, len(arts))
	for _, a := range arts {
		if !coveredByAny(stored, a.ArtIdx) {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return nil
	}

	// Sort (kept is already offset-sorted from the slice above, since
	// filtering preserves order), then merge: combine the stored rows and
	// the surviving articles — each article as its own single-article Run
	// — into one sorted list and fold adjacent entries left to right. This
	// single pass handles both "new articles merge with each other" and
	// "new articles merge with a stored row they abut", including a new
	// run that bridges two stored rows, with one merge rule instead of two.
	combined := make([]Run, 0, len(stored)+len(kept))
	combined = append(combined, stored...)
	for _, a := range kept {
		combined = append(combined, Run{
			FileIdx:     fileIdx,
			FirstArtIdx: a.ArtIdx,
			LastArtIdx:  a.ArtIdx,
			Offset:      a.Offset,
			Length:      int64(a.Length),
			CRC32:       a.CRC32,
		})
	}
	sort.Slice(combined, func(i, j int) bool { return combined[i].Offset < combined[j].Offset })

	final := mergeAdjacentRuns(combined)

	// Delete every stored row this commit read — some may be unchanged,
	// but re-writing them is a bounded, atomic no-op; leaving a stale
	// offset key behind (when a row merged leftward under a different
	// offset) would not be.
	if len(stored) > 0 {
		if err := s.deleteRows(ctx, tx, jobID, fileIdx, stored); err != nil {
			return err
		}
	}
	return s.insertRuns(ctx, tx, jobID, final)
}

// coveredByAny reports whether artIdx falls within any stored run's
// [FirstArtIdx, LastArtIdx] span.
func coveredByAny(stored []Run, artIdx int32) bool {
	for _, r := range stored {
		if artIdx >= r.FirstArtIdx && artIdx <= r.LastArtIdx {
			return true
		}
	}
	return false
}

// mergeAdjacentRuns folds a slice of Runs, already sorted by Offset, into
// maximal runs. Two entries merge only when they abut in BOTH byte offset
// and article index — offset contiguity or index contiguity alone is not
// enough (run.go's Commit doc; the design doc's core rule).
//
// The trap this exists to avoid: crc32util.Combine(a, b, lenB) requires lenB
// to be the WHOLE length of the run being folded in, not one article's
// length. Using r.Length — which already reflects any prior merging of r —
// is what keeps that true regardless of how many articles r itself covers.
func mergeAdjacentRuns(sorted []Run) []Run {
	if len(sorted) == 0 {
		return nil
	}
	out := make([]Run, 0, len(sorted))
	cur := sorted[0]
	for _, r := range sorted[1:] {
		if r.Offset == cur.Offset+cur.Length && r.FirstArtIdx == cur.LastArtIdx+1 {
			cur.CRC32 = crc32util.Combine(cur.CRC32, r.CRC32, r.Length)
			cur.Length += r.Length
			cur.LastArtIdx = r.LastArtIdx
			continue
		}
		out = append(out, cur)
		cur = r
	}
	out = append(out, cur)
	return out
}

// queryBracketing returns every stored run for (jobID, fileIdx) whose Offset
// is at or before maxEnd, ordered by Offset. A stored row starting after
// maxEnd cannot overlap or abut the incoming articles' offset span, so it is
// excluded from the read.
func (s *SQLiteRunStore) queryBracketing(ctx context.Context, tx *sql.Tx, jobID string, fileIdx int32, maxEnd int64) ([]Run, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT file_idx, first_art_idx, last_art_idx, offset, length, crc32
		  FROM durable_runs
		 WHERE job_id = ? AND file_idx = ? AND offset <= ?
		 ORDER BY offset`, jobID, fileIdx, maxEnd)
	if err != nil {
		return nil, fmt.Errorf("durability: query bracketing runs job=%s file=%d: %w", jobID, fileIdx, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.FileIdx, &r.FirstArtIdx, &r.LastArtIdx, &r.Offset, &r.Length, &r.CRC32); err != nil {
			return nil, fmt.Errorf("durability: scan bracketing run job=%s file=%d: %w", jobID, fileIdx, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// deleteRows removes exactly the stored rows Commit read, by their primary
// key. It is scoped to jobID/fileIdx by the caller's earlier query, but the
// full key is used here too so a delete can never remove a row this commit
// did not itself read and account for.
func (s *SQLiteRunStore) deleteRows(ctx context.Context, tx *sql.Tx, jobID string, fileIdx int32, rows []Run) error {
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM durable_runs WHERE job_id = ? AND file_idx = ? AND offset = ?`)
	if err != nil {
		return fmt.Errorf("durability: prepare run delete job=%s: %w", jobID, err)
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, jobID, fileIdx, r.Offset); err != nil {
			return fmt.Errorf("durability: delete run job=%s file=%d offset=%d: %w", jobID, fileIdx, r.Offset, err)
		}
	}
	return nil
}

// insertRuns writes the final merged runs for one file.
func (s *SQLiteRunStore) insertRuns(ctx context.Context, tx *sql.Tx, jobID string, runs []Run) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO durable_runs
			(job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("durability: prepare run insert job=%s: %w", jobID, err)
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range runs {
		if _, err := stmt.ExecContext(ctx, jobID, r.FileIdx, r.FirstArtIdx, r.LastArtIdx, r.Offset, r.Length, r.CRC32); err != nil {
			return fmt.Errorf("durability: insert run job=%s file=%d offset=%d: %w", jobID, r.FileIdx, r.Offset, err)
		}
	}
	return nil
}

// ForFile returns every stored run for one file, ordered by Offset.
func (s *SQLiteRunStore) ForFile(ctx context.Context, jobID string, fileIdx int32) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_idx, first_art_idx, last_art_idx, offset, length, crc32
		  FROM durable_runs WHERE job_id = ? AND file_idx = ? ORDER BY offset`, jobID, fileIdx)
	if err != nil {
		return nil, fmt.Errorf("durability: query runs job=%s file=%d: %w", jobID, fileIdx, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.FileIdx, &r.FirstArtIdx, &r.LastArtIdx, &r.Offset, &r.Length, &r.CRC32); err != nil {
			return nil, fmt.Errorf("durability: scan run job=%s file=%d: %w", jobID, fileIdx, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ForJob returns every stored run for a job, ordered by FileIdx then Offset.
func (s *SQLiteRunStore) ForJob(ctx context.Context, jobID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_idx, first_art_idx, last_art_idx, offset, length, crc32
		  FROM durable_runs WHERE job_id = ? ORDER BY file_idx, offset`, jobID)
	if err != nil {
		return nil, fmt.Errorf("durability: query runs job=%s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.FileIdx, &r.FirstArtIdx, &r.LastArtIdx, &r.Offset, &r.Length, &r.CRC32); err != nil {
			return nil, fmt.Errorf("durability: scan run job=%s: %w", jobID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteJob removes every run for a job that has left the queue. It touches
// only durable_runs — failed_articles is owned and written solely by
// internal/queue.
func (s *SQLiteRunStore) DeleteJob(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("durability: delete runs job=%s: %w", jobID, err)
	}
	return nil
}
