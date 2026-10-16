pkg ./internal/cmdutil/
run ^TestBuildCommand_ProcessGroupCancellationKillsChildren$

[WaitDelay default zeroed out]
file internal/cmdutil/runner.go
--- anchor
	if cmd.WaitDelay == 0 {
--- replace
	if false {
--- end

[WaitDelay overwrites custom non-zero setting]
file internal/cmdutil/runner.go
--- anchor
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 3 * time.Second
	}
--- replace
	cmd.WaitDelay = 3 * time.Second
--- end
