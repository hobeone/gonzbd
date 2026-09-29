pkg ./internal/postproc/
run TestLayoutB_ExtractedFileIsVerifiedAgainstPar2|TestLayoutB_UnrepairableExtractionFailsTheJob|TestLayoutB_IdentifiedSidecarStillDefersToUnpack|TestLayoutB_SetBesideAnOrdinarySetDefersOnlyItself|TestExtractedRepairStage_Skips|TestExtractedRepairStage_MissingDeferredSetFailsTheJob|TestQuickCheckStage_VerdictExcludesDeferredSets|TestRepairStage_SkipsDeferredSets

# The pass after unpack. Without it a Layout B post's extracted files are
# checked only by the archive, and a BLAKE2sp-only RAR checks nothing.
[extracted_repair never runs]
file internal/postproc/stage_extracted_repair.go
--- anchor
	if len(job.DeferredPar2Sets) == 0 {
--- replace
	if true {
--- end

[extracted_repair runs after a failed repair]
file internal/postproc/stage_extracted_repair.go
--- anchor
	case job.ParError:
--- replace
	case false:
--- end

[extracted_repair runs after a failed unpack]
file internal/postproc/stage_extracted_repair.go
--- anchor
	case job.UnpackError:
--- replace
	case false:
--- end

[a deferred set that is gone counts as verified]
file internal/postproc/stage_extracted_repair.go
--- anchor
	if ran < len(job.DeferredPar2Sets) {
--- replace
	if false {
--- end

[a verified set is never recorded as verified]
file internal/postproc/stage_extracted_repair.go
--- anchor
	job.DeferredPar2Verified = true
--- replace
--- end

# par2_cleanup deletes the par2 set only once it has verified the files.
[par2_cleanup keeps a verified deferred set]
file internal/postproc/stage_par2cleanup.go
--- anchor
	if len(job.DeferredPar2Sets) > 0 && !job.DeferredPar2Verified {
--- replace
	if len(job.DeferredPar2Sets) > 0 {
--- end

# repair's side of the per-set split: without the filter it runs a deferred
# set before unpack, and its failure skips unpack.
[repair ignores the deferral]
file internal/postproc/stage_repair.go
--- anchor
		if job.par2Deferred(set.Name) != deferred {
--- replace
		if false {
--- end

# quickcheck's side: the deferral is judged but never recorded.
[quickcheck does not record the deferred sets]
file internal/postproc/stage_quickcheck.go
--- anchor
	job.DeferredPar2Sets = deferred
--- replace
	job.DeferredPar2Sets = nil
--- end

# The verdict read over every set, deferred ones included: a deferred set's
# unaccounted entry makes an otherwise clean job Damaged.
[the verdict includes deferred sets]
file internal/postproc/stage_quickcheck.go
--- anchor
	crcResult := a.CRCExcluding(skip, log)
--- replace
	crcResult := a.CRC
--- end

# Unidentified only when every set with entries is deferred; otherwise repair
# is skipped for the ordinary set beside a Layout B one.
[unidentified when any set is deferred]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(deferred) > 0 && len(deferred) == len(setsWithEntries(a.ID)):
--- replace
	case len(deferred) > 0:
--- end

[sets with only identified entries are not counted]
file internal/postproc/stage_quickcheck.go
--- anchor
		sets[f.Desc.Set] = true
--- replace
		_ = f
--- end
