pkg ./internal/dispatch/
run TestDispatcher_Mutators$

[SetPP leaves the job's policy alone]
file internal/dispatch/registry.go
--- anchor
	e.j.SetPolicy(pp.Policy())
--- replace
	e.j.SetPolicy(job.Policy{})
--- end

[SetPP leaves the header's level alone]
file internal/dispatch/registry.go
--- anchor
	e.h.PP = pp
--- replace
	_ = pp
--- end
