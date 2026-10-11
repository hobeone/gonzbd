pkg ./internal/api/
run TestQueuePriority_|TestAddFile_PausedPriority|TestAddFile_RefusesNonPriorities|TestAddURL_RefusesNonPriority|TestQueueChangeCat_PausedCategoryPauses|TestPriorityParam

[name=priority stores the request unresolved]
file internal/api/queue.go
--- anchor
	pri, pause := config.ResolvePriority(cats, category, requested)
--- replace
	pri, pause, _ := requested, requested == constants.PausedPriority, cats
--- end

[name=priority does not pause on a Paused request]
file internal/api/queue.go
--- anchor
	if pause {
		if err := s.dispatcher.PauseJob(id); err != nil {
--- replace
	if false {
		if err := s.dispatcher.PauseJob(id); err != nil {
--- end

[name=priority accepts what parsePriority refuses]
file internal/api/queue.go
--- anchor
	requested, err := parsePriority(raw)
	if err != nil {
--- replace
	requested, err := parsePriority(raw)
	if false {
--- end

[addurl accepts what parsePriority refuses]
file internal/api/queue.go
--- anchor
	urlStr, ok := s.requireParam(w, r, "name", "URL")
	if !ok {
		return
	}
	priority, err := priorityParam(r)
	if err != nil {
--- replace
	urlStr, ok := s.requireParam(w, r, "name", "URL")
	if !ok {
		return
	}
	priority, err := priorityParam(r)
	if false {
--- end

[addfile and addlocalfile accept what parsePriority refuses]
file internal/api/queue.go
--- anchor
	priority, err := priorityParam(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := nzb.Parse(bytes.NewReader(data))
--- replace
	priority, err := priorityParam(r)
	if false {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := nzb.Parse(bytes.NewReader(data))
--- end

[parsePriority admits every int8]
file internal/api/helpers.go
--- anchor
	if !p.IsConcrete() && p != constants.PausedPriority && p != constants.DefaultPriority {
--- replace
	if false {
--- end

[parsePriority admits Stop]
file internal/api/helpers.go
--- anchor
	if !p.IsConcrete() && p != constants.PausedPriority && p != constants.DefaultPriority {
--- replace
	if !p.IsConcrete() && p != constants.PausedPriority && p != constants.DefaultPriority && p != constants.StopPriority {
--- end

[parsePriority admits Repair]
file internal/api/helpers.go
--- anchor
	if p == constants.RepairPriority {
--- replace
	if false {
--- end

[parsePriority truncates out-of-range values]
file internal/api/helpers.go
--- anchor
	if err != nil || n < -128 || n > 127 {
--- replace
	if err != nil {
--- end

[the response reports position 0]
file internal/api/queue.go
--- anchor
		"position": s.listingPosition(nzoID),
--- replace
		"position": 0,
--- end
