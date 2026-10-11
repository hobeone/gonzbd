pkg ./internal/api/
run TestQueueChangeCat_DefaultPriorityResolves|TestQueueChangeCat_Success|TestQueueChangeCat_PausedCategoryPauses

[change_cat stores the category's raw priority]
file internal/api/queue.go
--- anchor
		if _, _, err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
--- replace
		if err := s.dispatcher.SetPriority(nzoID, resolvedCat.Priority); err != nil {
--- end

[change_cat does not resolve through the category]
file internal/api/queue.go
--- anchor
		if _, _, err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
--- replace
		if _, _, err := s.applyPriority(nzoID, cat, constants.NormalPriority); err != nil {
--- end
