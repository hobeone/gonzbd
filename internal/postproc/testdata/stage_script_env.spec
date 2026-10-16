pkg ./internal/postproc/
run TestScriptStage_PassesPasswordAndPriority$

[password is omitted from script input]
file internal/postproc/stage_script.go
--- anchor
		Password:    job.Password,
--- replace
		Password:    "",
--- end

[priority is omitted from script input]
file internal/postproc/stage_script.go
--- anchor
		Priority:    job.Priority,
--- replace
		Priority:    0,
--- end
