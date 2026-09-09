pkg ./internal/job/
run TestAttachContent_RefusesASecondAttach|TestRestoreProgressState_FiltersStampsTheSameWayBeforeAndAfterHydration

[the re-attach guard neutered, so a second AttachContent replaces the live record]
file internal/job/content.go
--- anchor
	if j.progress != nil || j.manifest != nil {
		return fmt.Errorf("job %s: AttachContent: content already attached; use RestoreContent to install a manifest alongside existing progress", j.id)
	}
--- replace
	if false {
		return fmt.Errorf("job %s: AttachContent: content already attached; use RestoreContent to install a manifest alongside existing progress", j.id)
	}
--- end

[the pre-hydration stamp filter dropped, so the two tiers disagree about a stamp this process could not have minted]
file internal/job/content.go
--- anchor
	j.restoredDLStarted = jobStampOrZero(started)
	j.restoredDLFinished = jobStampOrZero(finished)
--- replace
	j.restoredDLStarted = started
	j.restoredDLFinished = finished
--- end
