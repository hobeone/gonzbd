pkg ./internal/directunpack/
run ^(TestDirectUnpack_SetAddedAfterTheQueueEmptiedIsExtracted|TestDirectUnpack_WaitSettlesSetsWhoseFirstVolumeNeverArrived|TestDirectUnpack_AddAfterWaitIsRefused|TestDirectUnpack_WaitSettlesALateSetWithoutVolume1|TestDirectUnpack_ParkedRunWakes|TestDirectUnpack_AddAfterAStartedRunFinishedIsRefused)$

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

[a set run took without volume 1 waits for it after the input ended]
file internal/directunpack/directunpack.go
--- anchor
		if vol == 1 && d.inputEnded {
--- replace
		if false {
--- end

[Wait does not wake a run waiting for volume 1]
file internal/directunpack/directunpack.go
--- anchor
	signal(d.queued)
	signal(d.volumeReady)
	<-d.done
--- replace
	signal(d.queued)
	<-d.done
--- end

[Wait does not wake a parked run]
file internal/directunpack/directunpack.go
--- anchor
	signal(d.queued)
	signal(d.volumeReady)
	<-d.done
--- replace
	signal(d.volumeReady)
	<-d.done
--- end

[Add does not wake a parked run]
file internal/directunpack/directunpack.go
--- anchor
	if needWake {
		signal(d.queued)
	}
--- replace
	_ = needWake
--- end

[Abort does not wake a parked run]
file internal/directunpack/directunpack.go
--- anchor
	signal(d.volumeReady)
	signal(d.queued)

	// Wait for the goroutine to exit.
--- replace
	signal(d.volumeReady)

	// Wait for the goroutine to exit.
--- end
