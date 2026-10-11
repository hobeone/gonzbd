pkg ./internal/app/
run TestHandleFileUntrusted_AFailedDeleteIsOwedUntilItLands|TestRecorder_UntrustWhoseWriteFailsOwesTheDeleteToTheNextFlush|TestRecorder_OwedDeleteIsWrittenOnItsOwn|TestRecorder_DropsTheDebtOfAReplacedInstance|TestRecorder_UntrustThatOverransASuccessfulFlushIsPaidByTheNext|TestRecorder_RemergePutsBackNothingAnUntrustOverran|TestRecorder_RemergePutsBackOnlyWhatNoUntrustOverran|TestRecorder_ASecondUntrustDuringTheFlushKeepsItsDebt

# #827: an untrust whose delete does not land is owed by the recorder and
# carried, ahead of the file's rows, by every later write for the instance.
# Each mutation removes one half of that; each must be caught by a test that
# reads the record, not timing.

[the untrust is the old apply, which forgets a failed delete]
file internal/app/record.go
--- anchor
	if err := app.recorder.untrust(ctx, j, fileIdx); err != nil {
--- replace
	if err := app.recorder.apply(ctx, j, []durability.FileVerdict{{FileIdx: fileIdx, DeleteAll: true, ClearComplete: true}}); err != nil {
--- end

[the untrust does not record the delete as owed]
file internal/app/record.go
--- anchor
	r.owed[j][fileIdx] = seq
--- replace
	_ = seq
--- end

[the untrust waits for the lock before purging the file's buffer]
file internal/app/record.go
--- anchor
	r.purgeLocked(j, v)
	r.owedSeq++
--- replace
	r.owedSeq++
--- end

[the flush does not carry the owed delete]
file internal/app/record.go
--- anchor
			b.Verdicts = append(b.Verdicts, v)
--- replace
			_, _ = b, v
--- end

[the owed ClearComplete overrides the file's newer state in the batch]
file internal/app/record.go
--- anchor
				v.ClearComplete = false
--- replace
				v.ClearComplete = true
--- end

[a settled debt is cleared whichever untrust owes it]
file internal/app/record.go
--- anchor
	if cur, ok := r.owed[j][fileIdx]; !ok || cur != seq {
--- replace
	if _, ok := r.owed[j][fileIdx]; !ok || seq == 0 {
--- end

[the remerge puts back rows an untrust overran]
file internal/app/record.go
--- anchor
			if !r.overrunLocked(j, carried[j], row.FileIdx) {
--- replace
			if true {
--- end

[the remerge puts back the file state an untrust overran]
file internal/app/record.go
--- anchor
			if r.overrunLocked(j, carried[j], idx) {
--- replace
			if false {
--- end

[a replaced instance's debt is kept]
file internal/app/record.go
--- anchor
		if !current {
			delete(r.owed, j)
			continue
		}
--- replace
		if !current {
			continue
		}
--- end
