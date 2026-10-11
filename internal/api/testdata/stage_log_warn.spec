pkg ./internal/api/
run TestParseStageLog_WarnsOnlyOnCorruption

[malformed JSON is not logged]
file internal/api/history.go
--- anchor
		log.WarnContext(ctx, "history stage_log is not valid JSON", "id", nzoID, "error", err)
--- replace
		_ = log
--- end

[an empty stage log is treated as corruption]
file internal/api/history.go
--- anchor
	if raw == "" || raw == "null" {
--- replace
	if raw == "null" {
--- end
