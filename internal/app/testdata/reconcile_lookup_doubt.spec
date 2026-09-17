pkg ./internal/app/
run TestDropJobAlreadyInHistory_SkipsTheJobWhenTheHistoryLookupFails

# One mutation: doubt answered the way not-found is, which is the shape this
# branch was in before. Mutating the RETURN rather than the errors.Is, because
# neutering the condition leaves the history import unused and a compile error
# says nothing about whether the test would have caught the behaviour.

[doubt reports "not in history", so the caller finalizes a job that may already be filed]
file internal/app/durability.go
--- anchor
				"rather than risk finalizing one that is already filed",
				"job", jobID, "err", err)
			return true
--- replace
				"rather than risk finalizing one that is already filed",
				"job", jobID, "err", err)
			return false
--- end
