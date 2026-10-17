pkg ./scripts/localnntp/
run TestCompareScalar|TestSQLCondition|TestPollUntil|TestLogCondition|TestCLI_Wait

[every query error is treated as retryable]
run TestSQLCondition_ErrorClasses|TestPollUntil_StopsAtOnceOnFatalError|TestCLI_Wait
file scripts/localnntp/wait.go
--- anchor
	if ctx.Err() != nil || errors.Is(err, sql.ErrNoRows) {
--- replace
	if true {
--- end

[pollUntil keeps polling after a condition error]
run TestPollUntil_ReturnsConditionError|TestPollUntil_StopsAtOnceOnFatalError
file scripts/localnntp/wait.go
--- anchor
		if err != nil {
			return observed, false, err
		}
--- replace
		if err != nil && false {
			return observed, false, err
		}
--- end

[a SQL NULL is compared as the empty string]
run TestSQLCondition$|TestCLI_Wait
file scripts/localnntp/wait.go
--- anchor
	case nil:
		return "(NULL)", false, nil
--- replace
	case nil:
		got = ""
--- end

[NaN and the infinities pass as thresholds]
run TestCompareScalar|TestCLI_Wait
file scripts/localnntp/wait.go
--- anchor
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
--- replace
	if err != nil || (math.IsNaN(f) && math.IsInf(f, 0)) {
--- end

[--from-end starts the log scan at the beginning]
run TestLogCondition_FromEnd|TestCLI_Wait
file scripts/localnntp/wait.go
--- anchor
				c.offset = fi.Size()
--- replace
				c.offset = fi.Size() * 0
--- end

[a flag belonging to another mode is accepted]
run TestCLI_Wait
file scripts/localnntp/wait.go
--- anchor
			if w.set[name] {
--- replace
			if false {
--- end
