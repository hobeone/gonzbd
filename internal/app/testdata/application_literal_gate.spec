pkg ./internal/app/
run ^TestApplicationLiterals_OnlyInNew$

# New is the one constructor of an Application (#915). Each mutation builds one
# outside it, in each form the gate reads.

[a bare literal in a test of this package]
file internal/app/shutdown_stall_test.go
--- anchor
		unsetApp := newTestApplication(t)
--- replace
		unsetApp := &Application{}
--- end

[new(Application)]
file internal/app/shutdown_stall_test.go
--- anchor
		unsetApp := newTestApplication(t)
--- replace
		unsetApp := new(Application)
--- end

[a zero-value var]
file internal/app/shutdown_stall_test.go
--- anchor
		unsetApp := newTestApplication(t)
--- replace
		var unsetApp Application
--- end

[a qualified literal in another package]
file internal/api/statustests_test.go
--- anchor
	application, err := app.New(cfg, nil, app.WithPostProcStages([]postproc.Stage{}))
--- replace
	application, err := &app.Application{}, error(nil)
--- end
