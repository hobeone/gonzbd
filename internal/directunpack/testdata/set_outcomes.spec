pkg ./internal/directunpack/
run TestDirectUnpack_MultiVolume_OutOfOrder|TestSetAdmitted

# A set enters the queue once and is extracted once (#898).

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
