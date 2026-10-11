pkg ./internal/app/
run TestBuildIngestJob_CategoryDefaultPriorityResolves|TestBuildIngestJob_CategoryPriorityInherit

[ingest stores the requested priority unresolved]
file internal/app/ingest.go
--- anchor
	priority := config.ResolvePriority(categories, opts.Category, opts.Priority)
--- replace
	priority := opts.Priority
--- end
