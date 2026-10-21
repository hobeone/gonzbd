pkg ./internal/job/
run ^(TestAttachFiles_SeedSetsNoDoneBit|TestAttachFiles_NilSeedIsVerified|TestFileBytesDownloaded_EstimateIsBoundedAndInBytesUnits|TestInstallFileVerification_MarksVerifiedAndZeroesRecorded|TestRemainingBytes_UsesTheEstimateWhileUnverified|TestAttachFiles_RefusesASeedOfTheWrongLength|TestAttachFiles_SeededNonFetchedFileOwesNoPending|TestVerifiedWriters_MatchTheEnumerationStatedInProse)$

[the seed loop also sets a Done bit for the first Recorded articles]
file internal/job/progress.go
--- anchor
		f.Recorded = seed[fi].Recorded
--- replace
		f.Recorded = seed[fi].Recorded
		for k := 0; k < seed[fi].Recorded; k++ {
			p.done.Set(p.fileStarts[fi] + k)
		}
--- end

[fileBytesDownloaded ignores Verified and always estimates]
file internal/job/progress.go
--- anchor
	if f.Verified {
		return f.BytesDownloaded
	}
--- replace
--- end

[InstallFileVerification does not mark the file Verified]
file internal/job/verification.go
--- anchor
	p.files[fi].Verified = true
--- replace
--- end

[a nil seed yields unverified files]
file internal/job/progress.go
--- anchor
		f.Verified = seed == nil
--- replace
		f.Verified = false
--- end

[InstallFileVerification leaves Recorded standing]
file internal/job/verification.go
--- anchor
	p.files[fi].Recorded = 0
--- replace
--- end

[a seed of the wrong length is accepted]
file internal/job/content.go
--- anchor
	if seed != nil && len(seed) != ft.NumFiles() {
--- replace
	if false {
--- end

[a second function marks a file Verified]
file internal/job/content.go
--- anchor
func (p *JobProgress) setFileFetchPolicy(fi int, policy FetchPolicy) {
	f := &p.files[fi]
--- replace
func (p *JobProgress) setFileFetchPolicy(fi int, policy FetchPolicy) {
	f := &p.files[fi]
	f.Verified = true
--- end

[the seed's fetch policy is not applied]
file internal/job/progress.go
--- anchor
		p.setFileFetchPolicy(fi, seed[fi].Fetch)
--- replace
--- end

[sizeFigures reads BytesDownloaded directly]
file internal/job/progress.go
--- anchor
		if left := f.Bytes - p.fileBytesDownloaded(fi) - f.FailedBytes; left > 0 {
--- replace
		if left := f.Bytes - f.BytesDownloaded - f.FailedBytes; left > 0 {
--- end
