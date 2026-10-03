pkg ./internal/api/
run TestQueueRename_GoesThroughTheNameOwner

[the handler writes the name itself]
file internal/api/queue.go
--- anchor
	name, err := s.jobs.RenameJob(nzoID, name)
--- replace
	err := s.dispatcher.SetName(nzoID, name)
--- end

[a refused name is a server fault]
file internal/api/queue.go
--- anchor
	case errors.Is(err, app.ErrInvalidJobName), errors.Is(err, dispatch.ErrInvalidJobName):
--- replace
	case false && errors.Is(err, app.ErrInvalidJobName), errors.Is(err, dispatch.ErrInvalidJobName):
--- end

[the registry's refusal is a server fault]
file internal/api/queue.go
--- anchor
	case errors.Is(err, app.ErrInvalidJobName), errors.Is(err, dispatch.ErrInvalidJobName):
--- replace
	case errors.Is(err, app.ErrInvalidJobName):
--- end
