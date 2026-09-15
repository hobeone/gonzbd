pkg ./internal/app/
run TestSweepOrphanedDurability_ReclaimsOnlyWhatNothingCanReach|TestSweepOrphanedDurability_SparesEveryJobWhenAnIDIsNull|TestSweepOrphanedDurability_RollsBackWholeWhenOneJobRefuses

[the FAILED exception dropped, so a retry's truncate bound is destroyed]
file internal/app/durability.go
--- anchor
   AND NOT EXISTS (SELECT 1 FROM history h
                    WHERE h.nzo_id = owned.job_id AND h.status = ?)`
--- replace
   AND (? <> '')`
--- end

[the queue exception dropped, so a live download's ground is swept]
file internal/app/durability.go
--- anchor
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = owned.job_id)
--- replace
 WHERE 1 = 1
--- end

[the sweep collects nothing, so every orphan survives]
file internal/app/durability.go
--- anchor
		orphans = append(orphans, id)
--- replace
		_ = id
--- end

# Fidelity note, and the second time this trap has caught this branch. The
# small version of the mutation below — swap deleteJobDurabilityTx for
# deleteJobDurability and leave the surrounding transaction open — reports
# KILLED on SQLITE_BUSY, because opening a transaction inside an open one locks
# the file. That is a fact about SQLite, not about whether the test notices
# per-job commits, and it would keep reporting KILLED with the rollback
# assertion deleted. The faithful mutation reverts the whole reclaim to the
# shape it had before: no outer transaction, one per job.

[the sweep commits per job again, so a later refusal cannot undo earlier ones]
file internal/app/durability.go
--- anchor
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("app: sweep orphaned durability rows: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, id := range orphans {
		// Checked per job because the transaction now spans all of them: a
		// cancellation arriving mid-loop would otherwise keep issuing
		// statements against a transaction that cannot commit, turning one
		// cancellation into one failure per remaining orphan.
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("app: sweep orphaned durability rows: abandoned after %d of %d: %w",
				i, len(orphans), err)
		}
		if err := app.deleteJobDurabilityTx(ctx, tx, id); err != nil {
			return 0, fmt.Errorf("app: sweep orphaned durability rows: job %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("app: sweep orphaned durability rows: commit: %w", err)
	}
	return len(orphans), nil
--- replace
	swept := 0
	var errs []error
	for _, id := range orphans {
		if err := app.deleteJobDurability(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", id, err))
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
--- end

[NOT EXISTS relaxed to NOT IN, the NULL trap the comment names]
file internal/app/durability.go
--- anchor
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = owned.job_id)
   AND NOT EXISTS (SELECT 1 FROM history h
                    WHERE h.nzo_id = owned.job_id AND h.status = ?)`
--- replace
 WHERE owned.job_id NOT IN (SELECT d.id FROM dispatch_jobs d)
   AND owned.job_id NOT IN (SELECT h.nzo_id FROM history h WHERE h.status = ?)`
--- end
