pkg ./internal/api/
run TestQueueChangeOpts_|TestQueueChangeCat_

[change_opts accepts a negative pp]
file internal/api/queue.go
--- anchor
	if level < 0 {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("pp must not be negative, got %d", level))
--- replace
	if false {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("pp must not be negative, got %d", level))
--- end

[change_opts refuses a pp above 3 instead of saturating it]
file internal/api/queue.go
--- anchor
	if level < 0 {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("pp must not be negative, got %d", level))
--- replace
	if level < 0 || level > 3 {
		s.respondError(w, http.StatusBadRequest, fmt.Sprintf("pp must not be negative, got %d", level))
--- end

[change_opts drops the requested level]
file internal/api/queue.go
--- anchor
	pp := job.ClampPP(level)
--- replace
	pp := job.ClampPP(0)
--- end

[change_cat drops the category's pp]
file internal/api/queue.go
--- anchor
			s.dispatcher.SetPP(nzoID, job.ClampPP(resolvedCat.PP)),
--- replace
			s.dispatcher.SetPP(nzoID, job.ClampPP(0)),
--- end
