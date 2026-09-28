pkg ./internal/history/
run TestOpen_DoesNotBlockBehindAWriter

[a startup VACUUM reintroduced after migrations]
file internal/history/db.go
--- anchor
	if _, err := provider.Up(ctx); err != nil {
		_ = sqlDB.Close() // superseded by migration error
		return nil, fmt.Errorf("history: run migrations: %w", err)
	}
--- replace
	if _, err := provider.Up(ctx); err != nil {
		_ = sqlDB.Close() // superseded by migration error
		return nil, fmt.Errorf("history: run migrations: %w", err)
	}

	if _, err := sqlDB.ExecContext(ctx, "VACUUM"); err != nil {
		_ = sqlDB.Close() // superseded by vacuum error
		return nil, fmt.Errorf("history: VACUUM: %w", err)
	}
--- end
