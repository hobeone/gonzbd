pkg ./internal/app/
run Test(AppRunner_EveryStateReportsExactlyOnce|ReportRecorder_WakesAwaitCallsOnEachReport)$

[a report does not wake the waiter]
file internal/app/runner_test.go
--- anchor
	r.total++
	if r.changed != nil {
		close(r.changed)
		r.changed = nil
	}
--- replace
	r.total++
--- end
