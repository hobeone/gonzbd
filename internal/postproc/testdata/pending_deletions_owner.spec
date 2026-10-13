pkg ./internal/postproc/
run ^TestLoadPendingDeletions_|^TestDeferredCleanup_

[the sidecar load bypasses the gatekeeper]
file internal/postproc/stages.go
--- anchor
		j.recordPendingDeletion(p)
--- replace
		j.pendingDeletions = append(j.pendingDeletions, p)
--- end

[the sidecar load keeps the list it replaces]
file internal/postproc/stages.go
--- anchor
	j.clearPendingDeletions()
	for _, p := range paths {
--- replace
	for _, p := range paths {
--- end

[recovery ignores the sidecar]
file internal/postproc/postproc.go
--- anchor
		if err := job.loadPendingDeletions(data); err == nil {
--- replace
		if err := job.loadPendingDeletions(data[:0]); err == nil {
--- end

[finalize keeps the list it deleted]
file internal/postproc/stages.go
--- anchor
	j.pendingDeletions = nil
--- replace
	_ = j
--- end
