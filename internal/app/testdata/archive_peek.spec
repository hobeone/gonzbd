# Red check for the archive peek (internal/app/archive_peek.go): each mutation
# neuters one decision and a named test must die.
#
#     go run ./scripts/mutate internal/app/testdata/archive_peek.spec
pkg ./internal/app/
run TestPeek_|TestArchivePeek_|TestBlockForUnwanted_|TestReevaluateStall_ABlocked|TestArchiveMemberNames
timeout 10m

[a file with a failed article is read anyway]
file internal/app/archive_peek.go
--- anchor
	if p == nil || hasFailedArticle(m, p, fc.FileIdx) {
--- replace
	if p == nil {
--- end

[the off action does not stop the peek]
file internal/app/archive_peek.go
--- anchor
	if rules.Action() == unwanted.ActionOff {
--- replace
	if false {
--- end

[a par2 file is never identified]
file internal/app/archive_peek.go
--- anchor
	if !isPar2 {
--- replace
	if !isPar2 || true {
--- end

[a RAR is never identified by its magic]
file internal/app/archive_peek.go
--- anchor
	case err == nil && ver == 5:
--- replace
	case err == nil && ver == 5 && false:
--- end

[the pause action does not pause]
file internal/app/archive_peek.go
--- anchor
	moved, err := app.dispatcher.BlockUnwanted(j, action == unwanted.ActionPause)
--- replace
	moved, err := app.dispatcher.BlockUnwanted(j, false)
--- end

[the fail action does not file the job]
file internal/app/archive_peek.go
--- anchor
	if action == unwanted.ActionFail {
--- replace
	if false {
--- end

[the failed job is filed straight after the peek, before the file is marked complete]
file internal/app/app.go
--- anchor
		unwantedFail := app.peekArchiveForUnwanted(j, fc)
--- replace
		unwantedFail := app.peekArchiveForUnwanted(j, fc)
		if unwantedFail != "" {
			app.finalizeRegistered(j, unwantedFail, true)
			unwantedFail = ""
		}
--- end

[the failed job is never filed after the mark]
file internal/app/app.go
--- anchor
		if unwantedFail != "" {
			app.finalizeRegistered(j, unwantedFail, true)
--- replace
		if false && unwantedFail != "" {
			app.finalizeRegistered(j, unwantedFail, true)
--- end

[a par2 file that fails to parse is not reported as an error]
file internal/app/archive_peek.go
--- anchor
		return nil, "par2", fmt.Errorf("parse par2 file: %w", err)
--- replace
		_ = err
--- end

[the recorded filename is never used to locate the file]
file internal/app/archive_peek.go
--- anchor
	} else if name := p.FileFilename(fc.FileIdx); name != "" {
--- replace
	} else if name := p.FileFilename(fc.FileIdx); name != "" && false {
--- end

[a call that lost the race to block still acts]
file internal/app/archive_peek.go
--- anchor
	if !moved {
--- replace
	if false {
--- end

[a hit leaves the running unpacker alive]
file internal/app/archive_peek.go
--- anchor
	app.duOrch.abortJob(jobID)
--- replace
	_ = jobID
--- end

[the stall re-evaluation drops a blocked parked job's recovery]
file internal/app/stall.go
--- anchor
				if errors.Is(err, dispatch.ErrUnwantedBlocked) {
--- replace
				if errors.Is(err, dispatch.ErrUnwantedBlocked) && false {
--- end

[a retry does not read the entry's blocked standing]
file internal/app/unwanted.go
--- anchor
	if len(found) == 0 && !priorBlock {
--- replace
	if len(found) == 0 && (!priorBlock || true) {
--- end

[a retry does not pass the entry's blocked standing to the check]
file internal/app/app.go
--- anchor
allowUnwanted || entry.Unwanted == unwanted.StateApproved, entry.Unwanted == unwanted.StateBlocked)
--- replace
allowUnwanted || entry.Unwanted == unwanted.StateApproved, false)
--- end

[par2-declared archive volumes are judged]
file internal/app/archive_peek.go
--- anchor
		if unpack.Classify(f.FileName) != unpack.UnknownArchive || par2.IsPar2Name(f.FileName) {
--- replace
		if unpack.Classify(f.FileName) != unpack.UnknownArchive && false || par2.IsPar2Name(f.FileName) {
--- end

[par2-declared par2 files are judged]
file internal/app/archive_peek.go
--- anchor
		if unpack.Classify(f.FileName) != unpack.UnknownArchive || par2.IsPar2Name(f.FileName) {
--- replace
		if unpack.Classify(f.FileName) != unpack.UnknownArchive || par2.IsPar2Name(f.FileName) && false {
--- end

[a RAR3 volume is listed through the unrar fallback]
file internal/app/archive_peek.go
--- anchor
		return nil, "rar", errRAR3Skipped
--- replace
		info, ierr := rarheader.Inspect(path)
		return info.Filenames, "rar", ierr
--- end

[the par2 exemption is the substring test again, so evil.par2.exe is skipped]
file internal/app/archive_peek.go
--- anchor
|| par2.IsPar2Name(f.FileName) {
--- replace
|| job.IsPar2File(f.FileName) {
--- end

[the unpacker feed does not refuse a blocked job]
file internal/app/directunpack_orchestrator.go
--- anchor
	if st, _ := app.dispatcher.UnwantedState(fc.JobID); st == unwanted.StateBlocked {
--- replace
	if st, _ := app.dispatcher.UnwantedState(fc.JobID); st == unwanted.StateBlocked && false {
--- end
