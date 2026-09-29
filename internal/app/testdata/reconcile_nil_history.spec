pkg ./internal/app/
run TestDropJobAlreadyInHistory_DoesNothingWithoutAHistoryDatabase

# The guard lives inside dropJobAlreadyInHistory rather than at its call site,
# so it is the only thing standing between a history-less Application and a
# nil dereference. It has two disjuncts, and each is mutated on its own: the
# test's "no repository" case dies without the first, and its "repository, no
# database" case without the second.

[no nil-repository check, so a nil repository is dereferenced]
file internal/app/durability.go
--- anchor
	if app.historyRepo == nil || app.historyRepo.DB() == nil {
		return
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- replace
	if app.historyRepo.DB() == nil {
		return
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- end

[no nil-database check, so a repository with no database is queried]
file internal/app/durability.go
--- anchor
	if app.historyRepo == nil || app.historyRepo.DB() == nil {
		return
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- replace
	if app.historyRepo == nil {
		return
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- end
