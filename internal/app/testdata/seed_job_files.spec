pkg ./internal/app/
run TestSeedJobFiles

[the transaction neutered: a fault commits the prefix instead of rolling back]
file internal/app/app.go
--- anchor
			return fmt.Errorf("app: insert job_file %s index %d: %w", jobID, i, err)
--- replace
			break
--- end

[DO NOTHING turned into an upsert, so a re-seed clobbers checkpointed results]
file internal/app/app.go
--- anchor
ON CONFLICT(job_id, file_index) DO NOTHING`)
--- replace
ON CONFLICT(job_id, file_index) DO UPDATE SET filename = ''`)
--- end

[every row seeded at index 0 instead of the file's own index]
file internal/app/app.go
--- anchor
		if _, err := stmt.ExecContext(ctx, jobID, i); err != nil {
--- replace
		if _, err := stmt.ExecContext(ctx, jobID, 0); err != nil {
--- end
