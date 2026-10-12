pkg ./internal/api/
run TestQueuePriority_|TestAddFile_PausedPriority|TestAddFile_RefusesNonPriorities|TestAddURL_RefusesNonPriority|TestQueueChangeCat_|TestPriorityParam|TestParsePriority|TestApplyPriority_|TestListingPosition_

[applyPriority stores the request unresolved]
file internal/api/queue.go
--- anchor
	pri, pause, err := config.ResolvePriority(cats, category, requested)
--- replace
	pri, pause, err := requested, false, error(nil)
	_ = cats
--- end

[applyPriority ignores the resolver's error]
file internal/api/queue.go
--- anchor
	if err != nil {
		return err
	}
	if pause {
--- replace
	if false {
		return err
	}
	if pause {
--- end

[applyPriority does not pause when the resolution says to]
file internal/api/queue.go
--- anchor
	if pause {
		if err := s.dispatcher.PauseJob(id); err != nil {
--- replace
	if pause && false {
		if err := s.dispatcher.PauseJob(id); err != nil {
--- end

[applyPriority ignores a refused pause]
file internal/api/queue.go
--- anchor
		if err := s.dispatcher.PauseJob(id); err != nil {
--- replace
		if err := s.dispatcher.PauseJob(id); err != nil && false {
--- end

[applyPriority writes the priority before it pauses]
file internal/api/queue.go
--- anchor
	if pause {
		if err := s.dispatcher.PauseJob(id); err != nil {
			return err
		}
	}
	return s.dispatcher.SetPriority(id, int(pri))
--- replace
	if err := s.dispatcher.SetPriority(id, int(pri)); err != nil {
		return err
	}
	if pause {
		if err := s.dispatcher.PauseJob(id); err != nil {
			return err
		}
	}
	return nil
--- end

[applyPriority ignores SetPriority's error]
file internal/api/queue.go
--- anchor
	return s.dispatcher.SetPriority(id, int(pri))
--- replace
	_ = s.dispatcher.SetPriority(id, int(pri))
	return nil
--- end

[name=priority re-resolves a Paused request]
file internal/api/queue.go
--- anchor
	if requested == constants.PausedPriority {
--- replace
	if false {
--- end

[name=priority does not pause on a Paused request]
file internal/api/queue.go
--- anchor
		err = s.dispatcher.PauseJob(nzoID)
--- replace
		err = nil
--- end

[name=priority reports success for a failed write]
file internal/api/queue.go
--- anchor
	if err != nil {
		s.respondError(w, priorityErrorStatus(err), err.Error())
--- replace
	if err != nil && false {
		s.respondError(w, priorityErrorStatus(err), err.Error())
--- end

[an unknown job is a server error]
file internal/api/queue.go
--- anchor
	if errors.Is(err, dispatch.ErrNotFound) {
		return http.StatusNotFound
--- replace
	if false {
		return http.StatusNotFound
--- end

[listingPosition matches any row]
file internal/api/queue.go
--- anchor
	return slices.IndexFunc(s.dispatcher.List(), func(r dispatch.Row) bool { return r.ID == id })
--- replace
	return slices.IndexFunc(s.dispatcher.List(), func(r dispatch.Row) bool { return true })
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
