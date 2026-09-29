pkg ./internal/app/
run TestDropJobAlreadyInHistory_KeepsTheJobWhenTheHistoryLookupFails

# One mutation: doubt answered the way a found entry is, so the job is removed
# on a lookup that established nothing. Mutating the branch rather than the
# errors.Is, because neutering the condition leaves the history import unused
# and a compile error says nothing about whether the test would have caught
# the behaviour.

[doubt is treated as "in history", so a job that may not be filed is removed]
file internal/app/durability.go
--- anchor
				"job", jobID, "err", err)
		}
		return
	}
--- replace
				"job", jobID, "err", err)
			entry = &history.Entry{}
		} else {
			return
		}
	}
--- end
