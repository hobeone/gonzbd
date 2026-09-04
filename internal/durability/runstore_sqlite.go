package durability

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

// SQLiteRunStore persists the durable_runs record.
//
// It REPLACEs rather than appending, which is what retired R1's immutability:
// merging is read-modify-write by construction (§3.1 of the design doc), so a
// commit may delete and re-insert rows it owns. That is safe because a row is
// only ever written after a completed fsync — a later commit can only describe
// MORE durable bytes than an earlier one, never different ones for the same
// article. The append-only store this replaced needed immutability precisely
// because it wrote BEFORE the write, where a row could describe bytes that
// were never written.
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
func (s *SQLiteRunStore) Commit(ctx context.Context, jobID string, arts []DurableArticle) ([]Collision, error) {
	if len(arts) == 0 {
		return nil, nil
	}

	byFile := make(map[int32][]DurableArticle)
	for _, a := range arts {
		byFile[a.FileIdx] = append(byFile[a.FileIdx], a)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("durability: begin run commit job=%s: %w", jobID, err)
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

	var collisions []Collision
	for _, fi := range fileIdxs {
		found, err := s.commitFile(ctx, tx, jobID, fi, byFile[fi])
		if err != nil {
			return nil, err
		}
		collisions = append(collisions, found...)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("durability: commit run tx job=%s: %w", jobID, err)
	}
	// Below tx.Commit, so a collision is reported only once the drop it
	// describes is on stable storage. A rolled-back transaction dropped
	// nothing: the rows it would have replaced are still there, and the next
	// commit of the same articles re-derives the same collision from the same
	// inputs.
	return collisions, nil
}

// commitFile applies Commit's algorithm to one file's incoming articles,
// within the caller's transaction, and returns the exact-offset collisions the
// merge dropped.
func (s *SQLiteRunStore) commitFile(ctx context.Context, tx *sql.Tx, jobID string, fileIdx int32, arts []DurableArticle) ([]Collision, error) {
	slices.SortFunc(arts, func(a, b DurableArticle) int { return cmp.Compare(a.Offset, b.Offset) })

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
		return nil, err
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
		// Every incoming article was an at-least-once redelivery of one a
		// stored row already covers (R12). That is not a collision — no
		// entry is discarded here that asserted anything the record does
		// not already hold — so there is nothing to report and nothing to
		// write.
		return nil, nil
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
	// Offset ascending, then Length DESCENDING, then FirstArtIdx ascending.
	// All three keys are load-bearing and none is a stylistic preference.
	//
	// Offset is the fold's precondition. The other two exist because entries
	// can share an offset — a multi-part UU post asserts offset 0 for every
	// segment (internal/downloader/dispatch.go's UU block) — and two entries
	// at one offset never abut, so both survive the fold and both address the
	// one primary key (job_id, file_idx, offset). Under a bare Offset
	// comparison the sort is free to order them either way — slices.SortFunc
	// is not stable, exactly as sort.Slice was not — and INSERT OR REPLACE
	// then keeps whichever landed last.
	//
	// Length descending puts the longer entry first, which is the one
	// mergeAdjacentRuns keeps. FirstArtIdx breaks the remaining tie when two
	// entries share an offset AND a length, making the comparator a total
	// order — without it the accumulator at a shared offset is again
	// arbitrary, and picking the higher-indexed entry leaves the articles
	// that would have abutted the lower one unable to fold into it.
	slices.SortFunc(combined, func(a, b Run) int {
		return cmp.Or(
			cmp.Compare(a.Offset, b.Offset),
			cmp.Compare(b.Length, a.Length), // DESCENDING: operands reversed
			cmp.Compare(a.FirstArtIdx, b.FirstArtIdx),
		)
	})

	final, collisions := mergeAdjacentRuns(combined)

	// Delete every stored row this commit read — some may be unchanged,
	// but re-writing them is a bounded, atomic no-op; leaving a stale
	// offset key behind (when a row merged leftward under a different
	// offset) would not be.
	if len(stored) > 0 {
		if err := s.deleteRows(ctx, tx, jobID, fileIdx, stored); err != nil {
			return nil, err
		}
	}
	if err := s.insertRuns(ctx, tx, jobID, final); err != nil {
		return nil, err
	}
	// Collisions are returned only on the path that reached the insert. A
	// commitFile that failed earlier has changed nothing, and its caller
	// rolls the transaction back — reporting a drop from a transaction that
	// never landed would name an event that did not happen.
	return collisions, nil
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

// mergeAdjacentRuns folds a slice of Runs into maximal runs. Two entries
// merge when they abut in BOTH byte offset and article index — offset
// contiguity or index contiguity alone is not enough (run.go's Commit doc;
// the design doc's core rule).
//
// It also DROPS an entry that shares an offset with the one before it, which
// is not a merge and does not pretend to be: the two describe overlapping
// bytes, so no combined CRC over them would be meaningful. The input must be
// sorted by Offset ascending then Length DESCENDING (commitFile's sort says
// why), which puts the longer entry first and so makes the kept one the
// longer. That choice is deliberate. Both entries' bytes are on disk either
// way and the record cannot say which won, but FinalizeFile derives its
// truncate bound from the stored runs — so keeping the shorter would let a
// LATER finalize truncate the file to it, while keeping the longer leaves the
// bound conservative in the safe direction.
//
// The check is against cur's OWN offset, not against any offset cur has
// absorbed, and that asymmetry is deliberate rather than an oversight.
//
// Once cur has merged leftward its Offset stays at the start of the span while
// its Length grows, so a later entry landing on an INTERIOR offset of cur —
// the offset of an article already folded in — is not equal to cur.Offset, is
// not adjacent either, and becomes a row of its own. Two rows then describe
// overlapping bytes.
//
// That is the correct outcome, because the drop exists for exactly one reason:
// (job_id, file_idx, offset) is the primary key and cannot hold two rows at
// one offset. Two rows at DIFFERENT offsets violate nothing, so nothing forces
// a choice between them, and keeping both is what makes Σ length exceed the
// file's size — which is precisely the evidence §3.3's overlap check reports
// on. Dropping the later entry instead would erase that evidence, silence the
// warning, and return its article to Outstanding to be re-fetched and collide
// again.
//
// The consequence is that one physical situation — two articles claiming one
// offset — is reported as a Collision or as an overlap depending on whether
// the rival was absorbed by a merge first. Both reach the user; §3.5 withholds
// the whole-file CRC either way, on the row count in this case and on article
// coverage in the other.
//
// Every drop that DOES happen is reported, as a Collision, and this is the
// only place in the program that can report one. overlapFrom cannot: it compares Σ length
// against the file's size, and dropping the duplicate is exactly what stops
// Σ length from exceeding it. Nor can any later pass over the stored rows —
// the survivor is by then indistinguishable from a row that never had a
// rival. The information exists here and nowhere else, so it leaves by return
// value rather than being re-derived downstream.
//
// The trap this exists to avoid: crc32util.Combine(a, b, lenB) requires lenB
// to be the WHOLE length of the run being folded in, not one article's
// length. Using r.Length — which already reflects any prior merging of r —
// is what keeps that true regardless of how many articles r itself covers.
func mergeAdjacentRuns(sorted []Run) ([]Run, []Collision) {
	if len(sorted) == 0 {
		return nil, nil
	}
	out := make([]Run, 0, len(sorted))
	var collisions []Collision
	cur := sorted[0]
	for _, r := range sorted[1:] {
		// Checked before the fold, because a zero-length cur would satisfy
		// both predicates and folding an overlapping entry is never right.
		if r.Offset == cur.Offset {
			collisions = append(collisions, Collision{
				FileIdx: cur.FileIdx,
				Offset:  cur.Offset,
				Kept:    cur.FirstArtIdx,
				Dropped: r.FirstArtIdx,
			})
			continue
		}
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
	return out, collisions
}

// queryBracketing returns every stored run for (jobID, fileIdx) whose Offset
// is at or before maxEnd, ordered by Offset. A stored row starting after
// maxEnd cannot overlap or abut the incoming articles' offset span, so it is
// excluded from the read.
//
// The absence of a LOWER bound is deliberate, and it is load-bearing rather
// than an oversight. The read is one-sided on purpose: a stored run that
// STARTS far below the incoming articles' offsets may still cover their
// article indices, because a run grows leftward across commits. A batch
// redelivering article 9 at offset 900 has to find the stored run [0,9],
// which starts at offset 0. An `offset >= minOffset` clause would hide that
// row, coveredByAny would report false, the redelivered article would be
// inserted as a second row, and Σ length would then exceed the file's real
// size — a false overlap on a healthy file, which is exactly what
// TestSQLiteRunStore_RedeliveredAdjacentToNewIsNotAFalseOverlap exists to
// prevent.
//
// So do not optimise this into a two-sided range. The upper bound is safe
// because a row starting past maxEnd can neither be covered by nor abut the
// incoming span; no symmetric argument holds at the low end.
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

// DeleteFile removes every run for one file of a job. See RunStore.DeleteFile
// for why the resume needs a file-scoped deletion rather than a job-scoped one.
func (s *SQLiteRunStore) DeleteFile(ctx context.Context, jobID string, fileIdx int32) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM durable_runs WHERE job_id = ? AND file_idx = ?`, jobID, fileIdx); err != nil {
		return fmt.Errorf("durability: delete runs job=%s file=%d: %w", jobID, fileIdx, err)
	}
	return nil
}

// DeleteJob removes every run for a job that has left the queue. It touches
// only durable_runs — failed_articles is owned and written solely by
// checkpoint.Store.SaveBatch.
func (s *SQLiteRunStore) DeleteJob(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("durability: delete runs job=%s: %w", jobID, err)
	}
	return nil
}
