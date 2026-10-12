pkg ./internal/api/
run TestQueueChangeOpts_|TestQueueChangeCat_

[change_opts saturates pp instead of refusing it]
file internal/api/queue.go
--- anchor
	pp, err := job.ParsePP(intParam(r, "value2"))
--- replace
	pp, err := job.ClampPP(intParam(r, "value2")), error(nil)
--- end

[change_cat drops the category's pp]
file internal/api/queue.go
--- anchor
			s.dispatcher.SetPP(nzoID, job.ClampPP(resolvedCat.PP)),
--- replace
			s.dispatcher.SetPP(nzoID, job.ClampPP(0)),
--- end
