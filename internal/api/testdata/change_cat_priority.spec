pkg ./internal/api/
run TestQueueChangeCat_DefaultPriorityResolves|TestQueueChangeCat_Success

[change_cat stores the category's raw priority]
file internal/api/queue.go
--- anchor
		_ = s.dispatcher.SetPriority(nzoID, int(config.ResolvePriority(cats, cat, constants.DefaultPriority)))
--- replace
		_ = s.dispatcher.SetPriority(nzoID, resolvedCat.Priority)
--- end
