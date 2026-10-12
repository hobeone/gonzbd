pkg ./internal/dispatch/
run TestCancelledNeverRunJob_ManifestIsEvicted

# Removing a cancelled never-run job evicts its manifest before deregistering
# it, as removeFor does (#863).

[the cancelled never-run job is deregistered without an eviction]
file internal/dispatch/tick.go
--- anchor
	d.res.Evict(j.ID())
	rm.end()
	return true
--- replace
	rm.end()
	return true
--- end
