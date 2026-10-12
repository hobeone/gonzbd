pkg ./internal/api/
run TestQueueChangeCat_

[change_cat stores the category's raw priority]
file internal/api/queue.go
--- anchor
		if err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
--- replace
		if err := s.dispatcher.SetPriority(nzoID, resolvedCat.Priority); err != nil {
--- end

[change_cat does not resolve through the category]
file internal/api/queue.go
--- anchor
		if err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
--- replace
		if err := s.applyPriority(nzoID, cat, constants.NormalPriority); err != nil {
--- end

[change_cat ignores a refused priority change]
file internal/api/queue.go
--- anchor
		if err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
--- replace
		if err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil && false {
--- end

[change_cat answers an unknown job 500]
file internal/api/queue.go
--- anchor
		if err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
			s.respondError(w, priorityErrorStatus(err), err.Error())
--- replace
		if err := s.applyPriority(nzoID, cat, constants.DefaultPriority); err != nil {
			s.respondError(w, http.StatusInternalServerError, err.Error())
--- end
