pkg ./internal/app/
run TestBuildIngestJob_PolicyMatchesPolicyFromPP$

# BuildIngestJob's Policy construction reverting to the hand-rolled struct
# literal that #653 replaced, which left Verify at its zero value (false)
# for every pp, including pp>=1 where job.PolicyFromPP sets it true.

[BuildIngestJob stops using the canonical job.PolicyFromPP translator]
file internal/app/ingest.go
--- anchor
	j := job.New(id, name, job.PolicyFromPP(pp))
--- replace
	j := job.New(id, name, job.Policy{Repair: pp >= types.PPRepair, Unpack: pp >= types.PPUnpack, Delete: pp >= types.PPDelete})
--- end
