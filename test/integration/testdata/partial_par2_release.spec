pkg ./test/integration/
tags integration
run ^(TestOnDemandPar2_PartialRelease_FetchesOnlyTheCoveringVolumes|TestOnDemandPar2_NoIndex_FetchesOneVolumeAsTheIndex|TestOnDemandPar2_CRCMismatch_FetchesOnlyTheCoveringVolumes|TestOnDemandPar2_LostTailArticle_FetchesOnlyTheCoveringVolumes)$

# One missing interior article of a data file, a par2 set with 1-, 2-, 4- and
# 8-block volumes. The job must fetch the +1 and +2 volumes only, and repair.
# A whole-file CRC mismatch and a lost tail article are sized by a block-level
# verify instead.

[the verdict releases every held volume instead of a sized cover]
file internal/app/app.go
--- anchor
		n, err := app.releaseRecoveryVolumes(j, reason, sizing)
--- replace
		sizing = job.Unsized()
		n, err := app.releaseRecoveryVolumes(j, reason, sizing)
--- end

[the release ignores the cover and frees every held volume]
file internal/job/release.go
--- anchor
		if cover, ok := cheapestCover(blocks, bytes, s.need); ok {
--- replace
		if cover, ok := cheapestCover(blocks, bytes, s.need); ok && false {
--- end

[with no index on disk every volume is fetched instead of one]
file internal/app/app.go
--- anchor
				sizing = job.NeedBlocks(1)
--- replace
				sizing = job.Unsized()
--- end

[the damaged slices of a file are not counted]
file internal/app/par2_release.go
--- anchor
			damaged += job.DamagedSlices(byFile[pos[i]], f.Desc.FileSize, sliceSize)
--- replace
			damaged += 0 * job.DamagedSlices(byFile[pos[i]], f.Desc.FileSize, sliceSize)
--- end

[a CRC mismatch or an unchecked entry is sized from the written rows alone]
file internal/app/par2_release.go
--- anchor
	verified := a.CRC.Mismatched > 0 || a.CRC.Unverified > 0
--- replace
	verified := false
--- end

[the block-level verify releases more than it needs]
file internal/app/par2_release.go
--- anchor
		verifiedNeed = res.BlocksNeeded
--- replace
		verifiedNeed = job.MaxPar2Slices + 1
--- end

[a CRC mismatch is sized from the written rows alone]
file internal/app/par2_release.go
--- anchor
	verified := a.CRC.Mismatched > 0 || a.CRC.Unverified > 0
--- replace
	verified := a.CRC.Unverified > 0
--- end

[an index that describes nothing is parsed in place of the volume fetched as one]
file internal/par2/par2.go
--- anchor
	if s.readable != "" {
--- replace
	if false {
--- end
