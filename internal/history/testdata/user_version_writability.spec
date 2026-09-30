pkg ./internal/history/
run TestOpen_ReadOnlyError_Migrated

[the writability check's write-error guard neutered]
file internal/history/db.go
--- anchor
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion)); err != nil {
--- replace
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion)); err != nil && false {
--- end
