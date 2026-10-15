pkg ./scripts/localnntp/
run TestVerify_Crash_MidPar2RepairOnCompleteFile

[verifyCompletePostProcFallback disabled in verifyCrashRow]
file scripts/localnntp/verify.go
--- anchor
	if err != nil && fr.Complete && verifyCompletePostProcFallback(downloadDir, candidateSubdirs, filename, rf, r) {
--- replace
	if false && err != nil && fr.Complete && verifyCompletePostProcFallback(downloadDir, candidateSubdirs, filename, rf, r) {
--- end

[postProcAllowedCRCs skips clean segment CRC from ReleaseManifest]
file scripts/localnntp/verify.go
--- anchor
		if seg.Offset == r.Offset && int64(seg.Length) == r.Length && seg.CRC32 != r.CRC32 {
--- replace
		if false && seg.Offset == r.Offset && int64(seg.Length) == r.Length && seg.CRC32 != r.CRC32 {
--- end
