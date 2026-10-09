pkg ./internal/app/
run TestScriptCanFail_StageWiring

[stages.go inverts script_can_fail]
file internal/app/stages.go
--- anchor
	scriptStage.SetScriptCanFail(scriptCanFail)
--- replace
	scriptStage.SetScriptCanFail(!scriptCanFail)
--- end

[stages.go hardcodes script_can_fail=false]
file internal/app/stages.go
--- anchor
	scriptStage.SetScriptCanFail(scriptCanFail)
--- replace
	scriptStage.SetScriptCanFail(false && scriptCanFail)
--- end

[stages.go hardcodes script_can_fail=true]
file internal/app/stages.go
--- anchor
	scriptStage.SetScriptCanFail(scriptCanFail)
--- replace
	scriptStage.SetScriptCanFail(true || scriptCanFail)
--- end

[reloader.go skips SetScriptCanFail]
file internal/app/reloader.go
--- anchor
		app.stages.Script.SetScriptCanFail(pp.ScriptCanFail)
--- replace
		_ = pp.ScriptCanFail
--- end

[reloader.go inverts script_can_fail]
file internal/app/reloader.go
--- anchor
		app.stages.Script.SetScriptCanFail(pp.ScriptCanFail)
--- replace
		app.stages.Script.SetScriptCanFail(!pp.ScriptCanFail)
--- end
