pkg ./internal/dispatch/
run TestLaunch_ReportBeforeClaimLeavesNoStrandedClaim$

[launch trusts the Running check it made before the claim]
file internal/dispatch/worker.go
--- anchor
	v := d.q.Render(j)
	if !v.Running || v.Intent != job.IntentRun {
		d.clearLaunched(j.ID())
		return
	}
--- replace
	v := d.q.Render(j)
--- end

[the failed re-check returns without releasing the claim it took]
file internal/dispatch/worker.go
--- anchor
		d.clearLaunched(j.ID())
		return
	}
	d.mu.Lock()
--- replace
		return
	}
	d.mu.Lock()
--- end
