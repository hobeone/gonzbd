pkg ./internal/durability/
run TestReclaim_|TestPerJobTables_|TestRuleStatement_|TestInTx_
timeout 3m

[a failed history entry no longer keeps its written_articles]
file internal/durability/reclaim.go
--- anchor
	{name: "written_articles", keptForFailedEntry: true},
--- replace
	{name: "written_articles"},
--- end

[a failed history entry no longer keeps its job_files]
file internal/durability/reclaim.go
--- anchor
	{name: "job_files", keptForFailedEntry: true},
--- replace
	{name: "job_files"},
--- end

[the rule ignores the queue]
file internal/durability/reclaim.go
--- anchor
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = ` + t.name + `.job_id)`
--- replace
 WHERE 1 = 1`
--- end

[the kept status is Completed, not Failed]
file internal/durability/reclaim.go
--- anchor
		args = append(args, string(constants.StatusFailed))
--- replace
		args = append(args, string(constants.StatusCompleted))
--- end

[NOT EXISTS becomes NOT IN]
file internal/durability/reclaim.go
--- anchor
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = ` + t.name + `.job_id)`
--- replace
 WHERE ` + t.name + `.job_id NOT IN (SELECT id FROM dispatch_jobs)`
--- end

[Reclaim ignores its id filter]
file internal/durability/reclaim.go
--- anchor
	if n > 0 {
--- replace
	if false {
--- end

[Reclaim names only the first chunk of ids]
file internal/durability/reclaim.go
--- anchor
		for chunk := range slices.Chunk(ids, reclaimChunk) {
--- replace
		for chunk := range slices.Chunk(ids[:min(len(ids), reclaimChunk)], reclaimChunk) {
--- end

[perJobTables drops a table]
file internal/durability/reclaim.go
--- anchor
	{name: "job_files", keptForFailedEntry: true},
--- replace
--- end

[a failed rule statement no longer rolls back the ones before it]
file internal/durability/reclaim.go
--- anchor
	if err := fn(tx); err != nil {
		return err
	}
--- replace
	_ = fn(tx)
--- end
