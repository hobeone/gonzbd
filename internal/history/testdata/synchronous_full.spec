pkg ./internal/history/
run TestOpen_SetsSynchronousFullOnPooledConnections$

[the DSN setting synchronous(NORMAL) instead of FULL]
file internal/history/db.go
--- anchor
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate"
--- replace
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
--- end

[the DSN setting synchronous(NORMAL) with FULL applied only once at Open]
file internal/history/db.go
--- anchor
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %q: %w", path, err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close() // superseded by ping error
		return nil, fmt.Errorf("error pinging database: %s, %w", path, err)
	}

	// WAL mode is database-scoped (persists on disk) — only needs
	// to run once, not per-connection.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
--- replace
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %q: %w", path, err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close() // superseded by ping error
		return nil, fmt.Errorf("error pinging database: %s, %w", path, err)
	}

	if _, err := sqlDB.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	// WAL mode is database-scoped (persists on disk) — only needs
	// to run once, not per-connection.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
--- end

[connection-scoped foreign_keys and busy_timeout applied once at Open instead of in the DSN]
file internal/history/db.go
--- anchor
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %q: %w", path, err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close() // superseded by ping error
		return nil, fmt.Errorf("error pinging database: %s, %w", path, err)
	}

	// WAL mode is database-scoped (persists on disk) — only needs
	// to run once, not per-connection.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
--- replace
	dsn := path + "?_pragma=synchronous(FULL)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %q: %w", path, err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close() // superseded by ping error
		return nil, fmt.Errorf("error pinging database: %s, %w", path, err)
	}

	if _, err := sqlDB.ExecContext(ctx, "PRAGMA foreign_keys=1; PRAGMA busy_timeout=5000"); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	// WAL mode is database-scoped (persists on disk) — only needs
	// to run once, not per-connection.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
--- end
