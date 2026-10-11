pkg ./internal/api/
run TestQueueList_StatusFromSlots

[any non-empty queue reads Downloading (#735)]
file internal/api/queue.go
--- anchor
		if r.View.Running && r.View.State == job.Fetching {
--- replace
		if r.ID != "" {
--- end

[a paused job at Fetching counts as downloading (#735)]
file internal/api/queue.go
--- anchor
		if r.View.Running && r.View.State == job.Fetching {
--- replace
		if r.View.State == job.Fetching {
--- end

[a running job past Fetching counts as downloading (#735)]
file internal/api/queue.go
--- anchor
		if r.View.Running && r.View.State == job.Fetching {
--- replace
		if r.View.Running {
--- end

[global pause no longer short-circuits]
file internal/api/queue.go
--- anchor
	if paused {
		return "Paused"
	}
	if len(rows) == 0 {
--- replace
	if false {
		return "Paused"
	}
	if len(rows) == 0 {
--- end

[empty queue no longer Idle]
file internal/api/queue.go
--- anchor
	if len(rows) == 0 {
		return "Idle"
--- replace
	if false {
		return "Idle"
--- end
