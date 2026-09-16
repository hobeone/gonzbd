pkg ./internal/app/
run TestDropJobAlreadyInHistory_KeepsEverythingWhenTheDispatcherRemoveFails

# Two mutations, because the guard has two halves and each fails differently:
# whether it aborts at all, and what it tells the caller when it does.

[the abort removed: a failed Remove falls through and destroys everything anyway]
file internal/app/durability.go
--- anchor
				"manifest and durability rows for the next startup to reconcile",
				"jobID", jobID, "err", rmErr)
			return true
--- replace
				"manifest and durability rows for the next startup to reconcile",
				"jobID", jobID, "err", rmErr)
--- end

[the abort reports "not handled", so the caller falls through to the state check and files a complete job a second time]
file internal/app/durability.go
--- anchor
				"jobID", jobID, "err", rmErr)
			return true
		}
	}
--- replace
				"jobID", jobID, "err", rmErr)
			return false
		}
	}
--- end
