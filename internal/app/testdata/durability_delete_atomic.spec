pkg ./internal/app/
run TestDeleteJobDurability_IsAtomicAndReportsFailure

# Fidelity note. An obvious mutation here — swap one tx.ExecContext for
# db.ExecContext — is NOT faithful to the code this replaced, and it passes for
# the wrong reason. Mixing an autocommit write with an open transaction on the
# same SQLite file returns SQLITE_BUSY, so the test dies on a lock error rather
# than on a stranded row, and would keep dying even if the atomicity assertion
# were deleted. The pre-#549 code opened no transaction at all and hit no lock.
#
# So the first mutation below reverts the whole function to the three
# independent autocommits it used to be. That is the state this change exists
# to leave, and it is the only shape in which the rows can actually be
# stranded.

[the three deletes revert to independent autocommits, as before #549]
file internal/app/durability.go
--- anchor
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("app: delete durability rows %s: begin: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM job_files WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("app: delete durability rows %s: job files: %w", jobID, err)
	}
	if err := app.dropJobDurabilityTx(ctx, tx, jobID); err != nil {
		return fmt.Errorf("app: delete durability rows %s: %w", jobID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("app: delete durability rows %s: commit: %w", jobID, err)
	}
	return nil
--- replace
	_, _ = db.ExecContext(ctx, `DELETE FROM job_files WHERE job_id = ?`, jobID)
	if app.runs != nil {
		if err := app.runs.DeleteJob(ctx, jobID); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM failed_articles WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	return nil
--- end

[the delete's failure is swallowed again, so the caller is told nothing]
file internal/app/durability.go
--- anchor
	if err := app.dropJobDurabilityTx(ctx, tx, jobID); err != nil {
		return fmt.Errorf("app: delete durability rows %s: %w", jobID, err)
	}
--- replace
	if err := app.dropJobDurabilityTx(ctx, tx, jobID); err != nil {
		return nil
	}
--- end
