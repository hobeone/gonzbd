pkg ./internal/app/
run TestBuildIngestJob_CategoryDefaultPriorityResolves|TestBuildIngestJob_CategoryPriorityInherit|TestBuildIngestJob_PausedPriority

[ingest stores the requested priority unresolved]
file internal/app/ingest.go
--- anchor
	priority, pause := config.ResolvePriority(categories, opts.Category, opts.Priority)
--- replace
	priority, pause := opts.Priority, opts.Priority == -2
--- end

[ingest does not pause on a Paused request]
file internal/app/ingest.go
--- anchor
	if pause {
--- replace
	if pause && false {
--- end
