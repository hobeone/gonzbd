pkg ./internal/directunpack/
run TestDirectUnpack_MultiVolume_OutOfOrder|TestSetAdmitted|TestSettleLocked|TestDirectUnpack_AbortKeepsASetPublishedBeforeIt|TestDirectUnpack_AbortBeforeTheVerdict|TestDirectUnpack_AbortBeforeRunStartsTheSet

# A set enters the queue once and is extracted once (#898), and it gets one
# outcome, which settleLocked records: a set that published is a success even
# when Abort follows (#902).

[the first set starts without leaving the queue]
file internal/directunpack/directunpack.go
--- anchor
		d.makeCurrentLocked(setname)
		d.started = true
--- replace
		d.curSetname = setname
		d.started = true
--- end

[a settled set is admitted again]
file internal/directunpack/directunpack.go
--- anchor
	return succeeded || failed || skipped
--- replace
	return false && (succeeded || failed || skipped)
--- end

[Abort clears the sets that already succeeded]
file internal/directunpack/directunpack.go
--- anchor
		d.settleLocked(s, SuccessSet{}, errNotStarted)
	}
	d.nextSets = nil
--- replace
		d.settleLocked(s, SuccessSet{}, errNotStarted)
	}
	d.nextSets = nil
	d.successSets = make(map[string]SuccessSet)
--- end

[Abort records the current set as failed]
file internal/directunpack/directunpack.go
--- anchor
		d.settleLocked(s, SuccessSet{}, errNotStarted)
	}
	d.nextSets = nil
--- replace
		d.settleLocked(s, SuccessSet{}, errNotStarted)
	}
	d.nextSets = nil
	if d.curSetname != "" {
		d.failedSets[d.curSetname] = FailedSet{Reason: "aborted"}
	}
--- end

[a set that published after Abort is recorded as aborted]
file internal/directunpack/directunpack.go
--- anchor
	switch {
	case err == nil:
		d.successSets[setname] = res
--- replace
	switch {
	case err == nil && !d.killed:
		d.successSets[setname] = res
--- end

[run exits without settling a set Abort reached first]
file internal/directunpack/directunpack.go
--- anchor
		d.mu.Lock()
		setname := d.curSetname
		d.mu.Unlock()
--- replace
		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return
		}
		setname := d.curSetname
		d.mu.Unlock()
--- end
