pkg ./internal/app/
run TestAppRunner_EveryStateReportsExactlyOnce

[fetching fails to report]
file internal/app/runner.go
--- anchor
	case job.Fetching:
		go r.runFetch(ctx, id)
--- replace
	case job.Fetching:
		// do not run or report
--- end
