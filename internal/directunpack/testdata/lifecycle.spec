pkg ./internal/directunpack/
run ^(TestDirectUnpack_SetAddedAfterTheQueueEmptiedIsExtracted|TestDirectUnpack_WaitSettlesSetsWhoseFirstVolumeNeverArrived|TestDirectUnpack_AddAfterWaitIsRefused)$

# Every set the unpacker admits gets an outcome: run waits for a set queued
# after the queue emptied until Wait ends the input, and Wait settles the sets
# of a run that never started.

[run exits as soon as the queue is empty]
file internal/directunpack/directunpack.go
--- anchor
		case d.inputEnded || ctx.Err() != nil:
--- replace
		default:
--- end

[an idle run still reports its last set as current]
file internal/directunpack/directunpack.go
--- anchor
		d.settleLocked(setname, res, err)
		d.curSetname = ""
--- replace
		d.settleLocked(setname, res, err)
--- end

[Wait leaves the queue of a run that never started unsettled]
file internal/directunpack/directunpack.go
--- anchor
			d.settleLocked(s, SuccessSet{}, errFirstVolumeMissing)
--- replace
			_ = s
--- end

[Add admits a volume after the unpacker finished]
file internal/directunpack/directunpack.go
--- anchor
	if d.killed || d.finished {
--- replace
	if d.killed {
--- end
