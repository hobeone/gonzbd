pkg ./scripts/check_coverage/
run TestRunPkgCoverage_FailureNamesTheFailingTest|TestRunPkgCoverage_ForwardsRealStderrOnBuildFailure|TestFormatTestFailure_FAILLineSurvivesTailTruncation|TestFormatTestFailure_TailTruncatedTo50LinesWhenNoFAILOrPanic|TestFormatTestFailure_PanicBlockSurvivesTailTruncation|TestFormatTestFailure_ForwardsNonEmptyStderr

[the stdout capture dropped, so a failing go test's --- FAIL lines never reach formatTestFailure]
file scripts/check_coverage/main.go
--- anchor
	cmd.Stdout = &stdout
--- replace
	_ = stdout
--- end

[the stderr argument dropped at the call site, so a child's real stderr never reaches formatTestFailure]
file scripts/check_coverage/main.go
--- anchor
		return fmt.Errorf("tests failed in package %s:\n%s", pkgDir, formatTestFailure(stdout.String(), stderr.String()))
--- replace
		return fmt.Errorf("tests failed in package %s:\n%s", pkgDir, formatTestFailure(stdout.String(), ""))
--- end

[the FAIL-line section neutered, so a FAIL line outside the tail window is lost]
file scripts/check_coverage/main.go
--- anchor
	if len(failLines) > 0 {
--- replace
	if false {
--- end

[the panic section neutered, so a panic outside the tail window is lost]
file scripts/check_coverage/main.go
--- anchor
	if panicIdx := strings.Index(stdout, "panic:"); panicIdx != -1 {
--- replace
	if panicIdx := strings.Index(stdout, "panic:"); false {
--- end

[the tail truncation neutered, so the full stdout -- not just the last 50 lines -- is kept]
file scripts/check_coverage/main.go
--- anchor
	if len(tail) > tailLines {
--- replace
	if false {
--- end

[the tail section dropped, so a failure with neither a FAIL line nor a panic has no evidence at all]
file scripts/check_coverage/main.go
--- anchor
	fmt.Fprintf(&b, "--- last %d lines of stdout ---\n%s\n", tailLines, strings.Join(tail, "\n"))
--- replace
	_ = tailLines
--- end

[the stderr section neutered, so non-empty stderr is dropped]
file scripts/check_coverage/main.go
--- anchor
	if strings.TrimSpace(stderr) != "" {
--- replace
	if false {
--- end
