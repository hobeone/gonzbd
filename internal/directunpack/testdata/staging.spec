pkg ./internal/directunpack/
run TestDirectUnpack_FailedSetLeavesNothingInTheJobDir|TestDirectUnpack_AbortedSetLeavesNothingInTheJobDir|TestDirectUnpack_FailedSetReextractedFlatHasNoDuplicate|TestDirectUnpack_SucceededSetIsPublishedToTheJobDir|TestDirectUnpack_FlatCollisionWithTheJobDirIsRenamed

# A set is extracted into a staging directory and published into the job
# directory only when it succeeds (#855). The first three mutations undo the
# failure half: nothing of a failed or aborted set may reach the job directory.

[DirectUnpack extracts into the job directory instead of the stage]
file internal/directunpack/directunpack.go
--- anchor
	root, err := os.OpenRoot(stageDir)
--- replace
	root, err := os.OpenRoot(d.extractDir)
--- end

[a failed extraction's stage is published anyway]
file internal/unpack/snapshot.go
--- anchor
	if err := extract(stageDir); err != nil {
		return nil, err
	}
--- replace
	if err := extract(stageDir); err != nil {
		_, _ = publishStagedExtraction(log, outDir, stageDir, opts)
		return nil, err
	}
--- end

[the stage is not removed]
file internal/unpack/snapshot.go
--- anchor
	defer removeStageDir(log, stageDir)
	if err := extract(stageDir); err != nil {
--- replace
	if err := extract(stageDir); err != nil {
--- end

# The success half: a set that succeeds is published, under the names
# ExtractedFiles lists, renaming a flat_unpack collision with the job directory
# rather than dropping the member.

[a succeeded set is not published]
file internal/unpack/snapshot.go
--- anchor
	published, err := publishStagedExtraction(log, outDir, stageDir, opts)
--- replace
	published, err := []string(nil), error(nil)
--- end

[ExtractedFiles lists names relative to the job directory]
file internal/directunpack/directunpack.go
--- anchor
			extractedFiles = append(extractedFiles, filepath.Join(d.extractDir, rel))
--- replace
			extractedFiles = append(extractedFiles, rel)
--- end

[publish skips a taken name under OneFolder]
file internal/unpack/snapshot.go
--- anchor
		if opts.OneFolder && !opts.OverwriteFiles {
--- replace
		if false {
--- end
