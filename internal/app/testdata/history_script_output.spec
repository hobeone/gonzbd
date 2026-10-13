pkg ./internal/app/
run ^TestBuildHistoryEntry_FilesTheScriptOutput$

[history drops the script line]
file internal/app/history_helper.go
--- anchor
		ScriptLine:   ppJob.ScriptLine(),
--- replace
		ScriptLine:   "",
--- end

[history drops the script log]
file internal/app/history_helper.go
--- anchor
		ScriptLog:    []byte(ppJob.ScriptLog()),
--- replace
		ScriptLog:    nil,
--- end
