pkg ./internal/app/
run TestDropJobAlreadyInHistory_KeepsEverythingWhenTheDispatcherRemoveFails

# A failed Remove must be reported as handled. What it keeps is reclaim's to
# decide, and step3_call_sites.spec pins that.

[a failed Remove reports "not handled", so the caller falls through to the state check and files a complete job a second time]
file internal/app/durability.go
--- anchor
	app.reclaim(delCtx, jobID)
	delCancel()
	return true
}
--- replace
	app.reclaim(delCtx, jobID)
	delCancel()
	return false
}
--- end
