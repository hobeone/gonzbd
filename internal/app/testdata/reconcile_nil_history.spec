pkg ./internal/app/
run TestDropJobAlreadyInHistory_AnswersFalseWithoutAHistoryDatabase

# The guard moved inside dropJobAlreadyInHistory from Start's call site, which
# no longer carries it -- so this is now the only thing standing between a
# history-less Application and a nil dereference.

[the guard neutered, so a nil history repository is dereferenced]
file internal/app/durability.go
--- anchor
	if app.historyRepo == nil || app.historyRepo.DB() == nil {
		return false
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- replace
	if false {
		return false
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
--- end
