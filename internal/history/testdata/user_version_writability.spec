pkg ./internal/history/
run TestOpen_ReadOnlyError_Migrated|TestOpen_PreservesUserVersion

[the writability check's write-error guard neutered]
file internal/history/db.go
--- anchor
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion)); err != nil {
--- replace
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion)); err != nil && false {
--- end

[the round-trip write mutates the value instead of restoring it]
file internal/history/db.go
--- anchor
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion)); err != nil {
--- replace
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion+1)); err != nil {
--- end
