pkg ./internal/api/
run TestQueueRename_GoesThroughTheNameOwner

[a started job answers 500 instead of 409]
file internal/api/queue.go
--- anchor
	case errors.Is(err, dispatch.ErrJobStarted):
--- replace
	case false && errors.Is(err, dispatch.ErrJobStarted):
--- end
