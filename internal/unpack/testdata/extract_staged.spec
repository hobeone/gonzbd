pkg ./internal/unpack/
run TestExtractStaged

# ExtractStaged publishes only a successful extraction, removes its stage
# either way, and under OneFolder renames a name outDir already holds rather
# than skipping it (#855).

[a failed extraction's stage is published anyway]
file internal/unpack/snapshot.go
--- anchor
	if err := extract(stageDir); err != nil {
		return nil, err
	}
--- replace
	if err := extract(stageDir); err != nil {
		_, _ = publishStagedExtraction(log, outDir, stageDir, opts, opts.OneFolder)
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

[a successful extraction is not published]
file internal/unpack/snapshot.go
--- anchor
	return publishStagedExtraction(log, outDir, stageDir, opts, opts.OneFolder)
--- replace
	return nil, nil
--- end

[ExtractStaged does not ask publish to rename a taken name]
file internal/unpack/snapshot.go
--- anchor
	return publishStagedExtraction(log, outDir, stageDir, opts, opts.OneFolder)
--- replace
	return publishStagedExtraction(log, outDir, stageDir, opts, false)
--- end

[publish renames a taken name whatever OneFolder says]
file internal/unpack/snapshot.go
--- anchor
	return publishStagedExtraction(log, outDir, stageDir, opts, opts.OneFolder)
--- replace
	return publishStagedExtraction(log, outDir, stageDir, opts, true)
--- end

[publish renames a taken name even with overwrite]
file internal/unpack/snapshot.go
--- anchor
		if renameTaken && !opts.OverwriteFiles {
--- replace
		if renameTaken {
--- end

[a failed stage cannot stop the extraction from running]
file internal/unpack/snapshot.go
--- anchor
	stageDir, err := prepareStageDir(outDir)
	if err != nil {
		return nil, err
	}
	defer removeStageDir(log, stageDir)
--- replace
	stageDir, _ := prepareStageDir(outDir)
	defer removeStageDir(log, stageDir)
--- end
