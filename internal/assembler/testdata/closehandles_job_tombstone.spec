pkg ./internal/assembler/
run Test(CloseJobHandles_TombstonesTheWholeJob|CloseJobHandles_TombstonesEvenWhenTheSyncFailed)$

# The close-handles arm tombstones the whole job, so an article in flight at
# the hand-off cannot create a file the job never opened.

[the close-handles arm sets no job-level tombstone]
file internal/assembler/assembler.go
--- anchor
		cancelledJobs[targetID] = struct{}{}
--- replace
		_ = targetID
--- end
