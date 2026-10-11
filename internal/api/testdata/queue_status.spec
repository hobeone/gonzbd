pkg ./internal/api/
run TestQueueList_StatusFromSlots|TestQueueStatus

[any non-empty queue reads Downloading (#735)]
file internal/api/queue.go
--- anchor
		case v.Running && v.State == job.Fetching:
--- replace
		case r.ID != "":
--- end

[a paused job at Fetching counts as downloading (#735)]
file internal/api/queue.go
--- anchor
		case v.Running && v.State == job.Fetching:
--- replace
		case v.State == job.Fetching:
--- end

[a running job past Fetching counts as downloading (#735)]
file internal/api/queue.go
--- anchor
		case v.Running && v.State == job.Fetching:
--- replace
		case v.Running:
--- end

[global pause no longer short-circuits]
file internal/api/queue.go
--- anchor
	if paused {
		return "Paused", false
	}
	var waiting, held bool
--- replace
	if false {
		return "Paused", false
	}
	var waiting, held bool
--- end

[a held job is not noticed]
file internal/api/queue.go
--- anchor
		case v.Reason.IsPause():
--- replace
		case false:
--- end

[a job waiting to start is not noticed]
file internal/api/queue.go
--- anchor
		case waitingToDownload(v):
--- replace
		case false:
--- end

[held outranks waiting]
file internal/api/queue.go
--- anchor
	case waiting:
		return "Downloading", false
	case held:
		return "Paused", false
--- replace
	case held:
		return "Paused", false
	case waiting:
		return "Downloading", false
--- end

[the fallback reads Paused, not Idle]
file internal/api/queue.go
--- anchor
		return "Paused", false
	}
	return "Idle", false
--- replace
		return "Paused", false
	}
	return "Paused", false
--- end

[a running job's Reason is read]
file internal/api/queue.go
--- anchor
		case v.Running || v.Outcome.IsSettled():
--- replace
		case v.Outcome.IsSettled():
--- end

[a settled job is classified]
file internal/api/queue.go
--- anchor
		case v.Running || v.Outcome.IsSettled():
--- replace
		case v.Running:
--- end

[a job whose Fetching has ended still counts as waiting]
file internal/api/queue.go
--- anchor
	return v.Next == job.StateUnset && (v.State == job.StateUnset || v.State == job.Fetching)
--- replace
	return v.State == job.StateUnset || v.State == job.Fetching
--- end

[a job at Fetching waiting for a lease is not waiting]
file internal/api/queue.go
--- anchor
	return v.Next == job.StateUnset && (v.State == job.StateUnset || v.State == job.Fetching)
--- replace
	return v.Next == job.StateUnset && v.State == job.StateUnset
--- end

[a job not yet started is not waiting]
file internal/api/queue.go
--- anchor
	return v.Next == job.StateUnset && (v.State == job.StateUnset || v.State == job.Fetching)
--- replace
	return v.Next == job.StateUnset && v.State == job.Fetching
--- end
