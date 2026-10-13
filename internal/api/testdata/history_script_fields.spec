pkg ./internal/api/
run ^TestHistoryDefault_ScriptLineButNoScriptLog$

[mode=history returns the stored script log]
file internal/api/history.go
--- anchor
			ScriptLine:   e.ScriptLine,
--- replace
			ScriptLine:   e.ScriptLine,
			ScriptLog:    string(e.ScriptLog),
--- end

[mode=history drops the script line]
file internal/api/history.go
--- anchor
			ScriptLine:   e.ScriptLine,
--- replace
			ScriptLine:   "",
--- end
