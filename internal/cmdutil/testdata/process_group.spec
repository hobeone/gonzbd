pkg ./internal/cmdutil/
run TestBuildCommand_ProcessGroupCancellationKillsChildren

[Setpgid disabled in SetProcessGroup]
file internal/cmdutil/runner.go
--- anchor
	cmd.SysProcAttr.Setpgid = true
--- replace
	cmd.SysProcAttr.Setpgid = false
--- end

[process-group signal targets positive pid instead of group]
file internal/cmdutil/runner.go
--- anchor
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
--- replace
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
--- end

[fallback Process.Kill removed when group kill fails]
file internal/cmdutil/runner.go
--- anchor
		_ = cmd.Process.Kill()
--- replace
		_ = err
--- end
