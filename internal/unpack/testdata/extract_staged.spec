pkg ./internal/unpack/
run TestExtractStaged|TestInProcessExtraction_CancelledLeavesNothing|TestGoTar_FailedExtractionChangesNothing

# ExtractStaged publishes only a successful extraction, removes its stage
# either way, and under OneFolder renames a name outDir already holds rather
# than skipping it (#855). Every in-process extractor writes into the stage
# extractInStage gives it, so a failed or cancelled attempt leaves nothing in
# outDir (#900).

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

[a successful extraction is not published]
file internal/unpack/snapshot.go
--- anchor
	published, err := publishStagedExtraction(log, outDir, stageDir, opts)
--- replace
	published, err := []string(nil), error(nil)
--- end

[publish does not rename a taken name under OneFolder]
file internal/unpack/snapshot.go
--- anchor
		if opts.OneFolder && !opts.OverwriteFiles {
--- replace
		if false {
--- end

[publish renames a taken name whatever OneFolder says]
file internal/unpack/snapshot.go
--- anchor
		if opts.OneFolder && !opts.OverwriteFiles {
--- replace
		if !opts.OverwriteFiles {
--- end

[publish renames a taken name even with overwrite]
file internal/unpack/snapshot.go
--- anchor
		if opts.OneFolder && !opts.OverwriteFiles {
--- replace
		if opts.OneFolder {
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

[go_unrar extracts into outDir instead of the stage]
file internal/unpack/go_unrar.go
--- anchor
		return goUnRAREngine(ctx, log, archive, stageDir, password, opts)
--- replace
		return goUnRAREngine(ctx, log, archive, outDir, password, opts)
--- end

[go_7z extracts into outDir instead of the stage]
file internal/unpack/go_sevenzip.go
--- anchor
			res, err = goSevenZipInternal(ctx, log, archive, stageDir, password, opts)
--- replace
			res, err = goSevenZipInternal(ctx, log, archive, outDir, password, opts)
--- end

[go_tar extracts into outDir instead of the stage]
file internal/unpack/go_tar.go
--- anchor
			res, err = goTarInternal(ctx, log, archive, stageDir, opts)
--- replace
			res, err = goTarInternal(ctx, log, archive, outDir, opts)
--- end
