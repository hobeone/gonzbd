pkg ./scripts/check_coverage/
run TestRunPkgCoverage_FailureNamesTheFailingTest

[the stdout capture dropped, so a failing go test's --- FAIL lines never reach formatTestFailure]
file scripts/check_coverage/main.go
--- anchor
	cmd.Stdout = &stdout
--- replace
	_ = stdout
--- end
