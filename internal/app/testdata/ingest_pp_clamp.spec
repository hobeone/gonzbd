pkg ./internal/app/
run TestBuildIngestJob_ClampsAnOutOfRangePP$

[BuildIngestJob refuses an out-of-range pp to download-only instead of saturating it]
file internal/app/ingest.go
--- anchor
	pp := job.ClampPP(level)
--- replace
	pp, _ := job.ParsePP(level)
--- end
