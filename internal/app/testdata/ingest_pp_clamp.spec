pkg ./internal/app/
run TestBuildIngestJob_ClampsAnOutOfRangePP$

[BuildIngestJob takes an out-of-range pp as download-only instead of saturating it]
file internal/app/ingest.go
--- anchor
	pp := job.ClampPP(level)
--- replace
	pp := job.ClampPP(level)
	if level > 3 {
		pp = job.ClampPP(0)
	}
--- end
