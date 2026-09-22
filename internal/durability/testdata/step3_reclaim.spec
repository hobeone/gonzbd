pkg ./internal/durability/
run TestReclaim_|TestPerJobTables_|TestRuleStatement_|TestInTx_
timeout 3m

[a failed history entry no longer keeps its durable_runs]
file internal/durability/reclaim.go
--- anchor
	{name: "durable_runs", keptForFailedEntry: true},
--- replace
	{name: "durable_runs"},
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
   AND job_id IN (SELECT value FROM json_each(?))`
--- replace
   AND ? IS NOT NULL`
--- end

[perJobTables drops a table]
file internal/durability/reclaim.go
--- anchor
	{name: "failed_articles"},
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
