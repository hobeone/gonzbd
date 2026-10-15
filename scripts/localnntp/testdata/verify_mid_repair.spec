pkg ./scripts/localnntp/
run TestVerify_Crash_MidPar2RepairOnCompleteFile

[resolveCompletePostProcFile disabled in verifyCrashFileRows]
file scripts/localnntp/verify.go
--- anchor
	if fr.Complete && resolveCompletePostProcFile(downloadDir, candidateSubdirs, openFiles, fileIdx, filename, rf, rows) {
--- replace
	if false && fr.Complete && resolveCompletePostProcFile(downloadDir, candidateSubdirs, openFiles, fileIdx, filename, rf, rows) {
--- end

[allowedRowCRCs accepts clean manifest CRC even without a .1 backup]
file scripts/localnntp/verify.go
--- anchor
	if !hasPar2Backup {
		return crcs
	}
--- replace
	if false {
		return crcs
	}
--- end

[verifyAllRowsAgainstCandidate succeeds on first matching row instead of all rows]
file scripts/localnntp/verify.go
--- anchor
		if !slices.Contains(crcs, got) {
			return false
		}
--- replace
		if slices.Contains(crcs, got) {
			return true
		}
--- end
