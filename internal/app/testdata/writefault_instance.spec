pkg ./internal/app/
run ^(TestStallAndFail_LeaveALaterInstanceAlone|TestHandleWriteFault_ActsOnTheAttemptRegisteredOnTheWorker)$

# A storage fault stalls or fails the attempt it belongs to, never a later
# attempt registered under the same ID (#914). The dispatcher doors' own
# instance checks are pinned in internal/dispatch by fault_instance_doors.spec.

[handleWriteFault fails whatever instance holds the ID when its goroutine runs]
file internal/app/durability.go
--- anchor
			app.Fail(j, f)
			return
--- replace
			cur, _ := app.dispatcher.Job(jobID)
			app.Fail(cur, f)
			return
--- end

[handleWriteFault stalls whatever instance holds the ID when its goroutine runs]
file internal/app/durability.go
--- anchor
		app.Stall(j, f)
	})
--- replace
		cur, _ := app.dispatcher.Job(jobID)
		app.Stall(cur, f)
	})
--- end

[stall records a fault for an attempt that has left the queue]
file internal/app/durability.go
--- anchor
	if !app.registered(j) {
		app.log.Info("storage fault for an attempt that has left the queue; nothing is parked",
--- replace
	if false {
		app.log.Info("storage fault for an attempt that has left the queue; nothing is parked",
--- end

# Fail's own registered check is not mutated here: a stale attempt's Fail
# reaches the ID-keyed stall clear only through finalizeRegistered, whose
# RowJob refuses it, and through SetFailReason, an instance door. That RowJob
# is pinned by runner_hopeless_instance.spec.

[registered asks about the ID, not the instance]
file internal/app/dispatcher_wiring.go
--- anchor
	_, ok := app.dispatcher.RowJob(j)
--- replace
	_, ok := app.dispatcher.Row(j.ID())
--- end
