pkg ./internal/durability/
run TestStore_|TestBarrier_CommitWrapCanFailOrObserveTheCommit

[an exported Commit reappears on Store]
file internal/durability/progress.go
--- anchor
// DiscardFileRows deletes a job's job_files rows.
--- replace
func (s *Store) Commit(ctx context.Context, jobID string, arts []DurableArticle) ([]Collision, error) {
	return s.commit(ctx, jobID, arts)
}

// DiscardFileRows deletes a job's job_files rows.
--- end

[Admit overwrites a row that already exists]
file internal/durability/progress.go
--- anchor
ON CONFLICT(job_id, file_index) DO NOTHING`)
--- replace
ON CONFLICT(job_id, file_index) DO UPDATE SET complete = 0, filename = '', assembled_crc32 = 0, fetch_policy = excluded.fetch_policy`)
--- end

[SaveProgress drops failed marks]
file internal/durability/progress.go
--- anchor
		for _, artIdx := range jp.FailedArticles {
--- replace
		for _, artIdx := range jp.FailedArticles[:0] {
--- end

[FileRows abandons the rest at a bad row]
file internal/durability/progress.go
--- anchor
			errs = append(errs, fmt.Errorf("durability: scan job_files %s: %w: %w", jobID, ErrIncomplete, err))
			continue
--- replace
			errs = append(errs, fmt.Errorf("durability: scan job_files %s: %w: %w", jobID, ErrIncomplete, err))
			break
--- end

[FailedArticles discards what it read before a bad row]
file internal/durability/progress.go
--- anchor
			return out, fmt.Errorf("durability: scan failed_articles %s: %w: %w", jobID, ErrIncomplete, err)
--- replace
			return nil, fmt.Errorf("durability: scan failed_articles %s: %w: %w", jobID, ErrIncomplete, err)
--- end

[FailedArticles reports a partial read as a plain failure]
file internal/durability/progress.go
--- anchor
			return out, fmt.Errorf("durability: scan failed_articles %s: %w: %w", jobID, ErrIncomplete, err)
--- replace
			return out, fmt.Errorf("durability: scan failed_articles %s: %w", jobID, err)
--- end

[DiscardFileRows is not scoped to its job]
file internal/durability/progress.go
--- anchor
	if _, err := s.db.ExecContext(ctx, `DELETE FROM job_files WHERE job_id = ?`, jobID); err != nil {
--- replace
	if _, err := s.db.ExecContext(ctx, `DELETE FROM job_files WHERE ? IS NOT NULL`, jobID); err != nil {
--- end

[DiscardFailedArticles is not scoped to its job]
file internal/durability/progress.go
--- anchor
	if _, err := s.db.ExecContext(ctx, `DELETE FROM failed_articles WHERE job_id = ?`, jobID); err != nil {
--- replace
	if _, err := s.db.ExecContext(ctx, `DELETE FROM failed_articles WHERE ? IS NOT NULL`, jobID); err != nil {
--- end

[a failed query claims to be a partial read]
file internal/durability/progress.go
--- anchor
		return nil, fmt.Errorf("durability: query failed_articles %s: %w", jobID, err)
--- replace
		return nil, fmt.Errorf("durability: query failed_articles %s: %w: %w", jobID, ErrIncomplete, err)
--- end

[ForJob reports an unscannable row as a partial read]
file internal/durability/store.go
--- anchor
			return nil, fmt.Errorf("durability: scan run job=%s: %w", jobID, err)
--- replace
			return nil, fmt.Errorf("durability: scan run job=%s: %w: %w", jobID, ErrIncomplete, err)
--- end

[the barrier ignores its CommitWrap]
file internal/durability/barrier.go
--- anchor
	if b.wrap == nil {
		return b.runs.commit(ctx, jobID, arts)
	}
--- replace
	if true {
		return b.runs.commit(ctx, jobID, arts)
	}
--- end

[DiscardRuns is not scoped to its job]
file internal/durability/store.go
--- anchor
	if _, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE job_id = ?`, jobID); err != nil {
--- replace
	if _, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE ? IS NOT NULL`, jobID); err != nil {
--- end

[DiscardRuns deletes only one of the job's files]
file internal/durability/store.go
--- anchor
	if _, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE job_id = ?`, jobID); err != nil {
--- replace
	if _, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE job_id = ? AND file_idx = 0`, jobID); err != nil {
--- end

[ForFile returns what it read before a bad row]
file internal/durability/store.go
--- anchor
			return nil, fmt.Errorf("durability: scan run job=%s file=%d: %w", jobID, fileIdx, err)
--- replace
			return out, fmt.Errorf("durability: scan run job=%s file=%d: %w", jobID, fileIdx, err)
--- end
