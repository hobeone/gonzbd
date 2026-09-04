pkg ./internal/checkpoint/
run TestCheckpointer_FailedFlushLeavesJobsMarked|TestCheckpointer_MarkDuringFailedFlushIsNotLost|TestCheckpointer_RemarkDuringFailedFlushNotOverwrittenWithStale|TestCheckpointer_PruneDuringFlushIsNotReMergedOnFailure

[the re-merge guard is inverted, so a failed batch's jobs are never restored]
file internal/checkpoint/checkpointer.go
--- anchor
				if _, remarked := c.dirty[id]; !remarked {
--- replace
				if _, remarked := c.dirty[id]; remarked {
--- end

[the re-merge overwrites the whole dirty set instead of merging into it, so a job marked while the write was failing is lost]
file internal/checkpoint/checkpointer.go
--- anchor
	if err != nil {
		for id, j := range batch {
			if _, stillInFlight := c.inFlight[id]; stillInFlight {
				if _, remarked := c.dirty[id]; !remarked {
					c.dirty[id] = j
				}
			}
		}
	}
--- replace
	if err != nil {
		c.dirty = batch
	}
--- end

[the re-merge guard is omitted, so a job remarked while the write was failing is overwritten with stale state]
file internal/checkpoint/checkpointer.go
--- anchor
				if _, remarked := c.dirty[id]; !remarked {
					c.dirty[id] = j
				}
--- replace
				c.dirty[id] = j
--- end

[the inFlight prune guard is omitted, so a pruned job is resurrected on failed flush]
file internal/checkpoint/checkpointer.go
--- anchor
			if _, stillInFlight := c.inFlight[id]; stillInFlight {
--- replace
			if true {
--- end

