pkg ./internal/app/
run TestBuildIngestJob_CategoryDefaultPriorityResolves|TestBuildIngestJob_CategoryPriorityInherit|TestBuildIngestJob_PausedPriority|TestBuildIngestJob_RefusesNonConcretePriority

[ingest stores the requested priority unresolved]
file internal/app/ingest.go
--- anchor
	priority, pause, err := config.ResolvePriority(categories, opts.Category, opts.Priority)
--- replace
	priority, pause, err := opts.Priority, opts.Priority == -2, error(nil)
--- end

[ingest does not pause on a Paused request]
file internal/app/ingest.go
--- anchor
	if pause {
--- replace
	if pause && false {
--- end

[ingest stores a priority the resolver refused]
file internal/app/ingest.go
--- anchor
	priority, pause, err := config.ResolvePriority(categories, opts.Category, opts.Priority)
	if err != nil {
--- replace
	priority, pause, err := config.ResolvePriority(categories, opts.Category, opts.Priority)
	if err != nil && false {
--- end
