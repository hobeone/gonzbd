pkg ./internal/history/
run TestOpen_DoesNotVacuum

[a startup VACUUM reintroduced after migrations]
file internal/history/db.go
--- anchor
	if _, err := provider.Up(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("history: run migrations: %w", err), sqlDB.Close())
	}
--- replace
	if _, err := provider.Up(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("history: run migrations: %w", err), sqlDB.Close())
	}

	if _, err := sqlDB.ExecContext(ctx, "VACUUM"); err != nil {
		return nil, errors.Join(fmt.Errorf("history: VACUUM: %w", err), sqlDB.Close())
	}
--- end
