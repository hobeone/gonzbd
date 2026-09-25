pkg ./internal/history/
run TestAdd_StoresRetainedProgressWithItsEntry|TestAdd_RollsBackTheEntryWhenItsProgressCannotBeStored|TestAdd_WithNoProgressStoresJustTheEntry|TestDelete_TakesRetainedProgressWithTheEntry

[the progress is written outside the entry's transaction, as it was before]
file internal/history/repository.go
--- anchor
	if err := addFileProgressTx(ctx, tx, e.NzoID, files); err != nil {
--- replace
	if err := addFileProgressTx(ctx, r.db, e.NzoID, files); err != nil {
--- end

[the progress write is skipped entirely]
file internal/history/repository.go
--- anchor
	for _, f := range files {
--- replace
	for _, f := range files[:0] {
--- end

[the entry is committed before its progress is written, so a failure cannot roll it back]
file internal/history/repository.go
--- anchor
	if err := addFileProgressTx(ctx, tx, e.NzoID, files); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
--- replace
	if err := tx.Commit(); err != nil {
--- end

[a replace absorbs a duplicate file index, as INSERT OR REPLACE did]
file internal/history/repository.go
--- anchor
INSERT INTO history_job_files
--- replace
INSERT OR REPLACE INTO history_job_files
--- end

[the read drops fetch_policy, as the app-side query did]
file internal/history/repository.go
--- anchor
SELECT file_index, complete, fetch_policy,
--- replace
SELECT file_index, complete, 0,
--- end

[a failed progress write still commits the entry, leaving it without its progress]
file internal/history/repository.go
--- anchor
	if err := addFileProgressTx(ctx, tx, e.NzoID, files); err != nil {
		return err
	}
--- replace
	if err := addFileProgressTx(ctx, tx, e.NzoID, files); err != nil {
		_ = tx.Commit()
		return err
	}
--- end
