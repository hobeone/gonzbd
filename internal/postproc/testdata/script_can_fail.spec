pkg ./internal/postproc/
run TestScriptStage_CanFail|TestScriptCanFail_True|TestScriptStage_ScriptCanFail|TestScriptStage_FailingScript

[script_can_fail inverted back to failing on false]
file internal/postproc/stage_script.go
--- anchor
			if !s.scriptCanFail.Load() {
--- replace
			if s.scriptCanFail.Load() {
--- end

[script_can_fail always swallows non-zero exit]
file internal/postproc/stage_script.go
--- anchor
			if !s.scriptCanFail.Load() {
--- replace
			if true {
--- end

[script_can_fail never swallows non-zero exit]
file internal/postproc/stage_script.go
--- anchor
			if !s.scriptCanFail.Load() {
--- replace
			if false {
--- end

[script_can_fail=true omits job.FailMsg]
file internal/postproc/stage_script.go
--- anchor
			if status == 0 {
--- replace
			if false && status == 0 {
--- end

[script_can_fail=true overwrites prior stage failure]
file internal/postproc/stage_script.go
--- anchor
			if status == 0 {
--- replace
			if true || status == 0 {
--- end
