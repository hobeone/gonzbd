pkg ./internal/api/
run TestBuildSlot_ProgressVerified|TestBuildQueueFiles_ReportsPerFileVerified

[the slot claims verified regardless of the job]
file internal/api/queue.go
--- anchor
		progressVerified = j.AllVerified()
--- replace
		progressVerified = true
--- end

[a file claims unverified regardless of its flag]
file internal/api/queue.go
--- anchor
			Verified:        p.FileVerified(fi),
--- replace
			Verified:        false,
--- end
