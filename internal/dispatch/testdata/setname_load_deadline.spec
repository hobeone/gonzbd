pkg ./internal/dispatch/
run TestSetName_LoadsUnderADeadline

[a rename hydrates with no deadline]
file internal/dispatch/registry.go
--- anchor
	ctx, cancel := context.WithTimeout(context.Background(), setNameLoadTimeout)
--- replace
	ctx, cancel := context.WithCancel(context.Background())
--- end
