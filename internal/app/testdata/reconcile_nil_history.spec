pkg ./internal/app/
run TestDropJobAlreadyInHistory_DoesNothingWithoutAHistoryDatabase

# The guard lives inside dropJobAlreadyInHistory rather than at its call site,
# so it is the only thing standing between a history-less Application and a
# nil dereference.

[the guard neutered, so a nil history repository is dereferenced]
file internal/app/durability.go
--- anchor
	if app.historyRepo == nil || app.historyRepo.DB() == nil {
		return
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- replace
	if false {
		return
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- end
