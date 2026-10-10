pkg ./internal/unpack/
run ^(Test(UnRAR|SevenZip)_OverwriteFiles|TestStagedSubprocessExtraction|TestExternalExtract_PasswordReachesSubprocess)$

[UnRAR appends -or when OverwriteFiles is false]
file internal/unpack/unrar.go
--- anchor
		args = append(args, "-o-") // skip existing files
--- replace
		args = append(args, "-o-", "-or")
--- end

[SevenZip appends -aou instead of -aos when OverwriteFiles is false]
file internal/unpack/sevenzip.go
--- anchor
		args = append(args, "-aos") // skip extracting of existing files
--- replace
		args = append(args, "-aou")
--- end

[UnRAR extracts directly into outDir instead of stageDir]
file internal/unpack/unrar.go
--- anchor
	execArgs := append(append([]string(nil), args...), archive.MainFile, stageDir+"/")
--- replace
	execArgs := append(append([]string(nil), args...), archive.MainFile, outDir+"/")
--- end

[SevenZip extracts directly into outDir instead of stageDir]
file internal/unpack/sevenzip.go
--- anchor
	execArgs := append(append([]string(nil), args...), archive.MainFile, "-o"+stageDir, sscFlag)
--- replace
	execArgs := append(append([]string(nil), args...), archive.MainFile, "-o"+outDir, sscFlag)
--- end

[UnRAR omits deferred os.RemoveAll(stageDir)]
file internal/unpack/unrar.go
--- anchor
	defer func() { _ = os.RemoveAll(stageDir) }()
--- replace
	_ = stageDir
--- end

[SevenZip omits deferred os.RemoveAll(stageDir)]
file internal/unpack/sevenzip.go
--- anchor
	defer func() { _ = os.RemoveAll(stageDir) }()
--- replace
	_ = stageDir
--- end

[UnRAR omits substituting real password into execArgs]
file internal/unpack/unrar.go
--- anchor
	if password != "" {
		execArgs[flagIdx] = "-p" + password
	}
--- replace
	if false && password != "" {
		execArgs[flagIdx] = "-p" + password
	}
--- end

[UnRAR shifts flagIdx by 1 when substituting password]
file internal/unpack/unrar.go
--- anchor
	if password != "" {
		execArgs[flagIdx] = "-p" + password
	}
--- replace
	if password != "" {
		execArgs[flagIdx+1] = "-p" + password
	}
--- end

[SevenZip omits substituting real password into execArgs]
file internal/unpack/sevenzip.go
--- anchor
	if password != "" {
		execArgs[flagIdx] = "-p" + password
	}
--- replace
	if false && password != "" {
		execArgs[flagIdx] = "-p" + password
	}
--- end

[SevenZip shifts flagIdx by 1 when substituting password]
file internal/unpack/sevenzip.go
--- anchor
	if password != "" {
		execArgs[flagIdx] = "-p" + password
	}
--- replace
	if password != "" {
		execArgs[flagIdx+1] = "-p" + password
	}
--- end

[publishStagedExtraction overwrites existing files when overwrite is false]
file internal/unpack/snapshot.go
--- anchor
		if !opts.OverwriteFiles {
--- replace
		if false && !opts.OverwriteFiles {
--- end

[publishStagedExtraction skips log.Info when skipping existing file]
file internal/unpack/snapshot.go
--- anchor
				log.Info("skipping existing file", "path", filepath.Join(outDir, rel))
--- replace
				_ = log
--- end

[publishStagedExtraction skips OnLine callback when skipping existing file]
file internal/unpack/snapshot.go
--- anchor
				if opts.OnLine != nil {
					opts.OnLine("Skipping existing: " + rel)
				}
--- replace
				if false && opts.OnLine != nil {
					opts.OnLine("Skipping existing: " + rel)
				}
--- end


