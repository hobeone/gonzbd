pkg ./internal/cmdutil/
run TestBuildCommand_ProcessGroupCancellationKillsChildren

[Setpgid disabled in configureProcessGroup]
file internal/cmdutil/runner.go
--- anchor
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
--- replace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: false}
--- end

[process-group signal targets positive pid instead of group]
file internal/cmdutil/runner.go
--- anchor
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
--- replace
			_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
--- end
