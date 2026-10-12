pkg ./internal/unpack/
run ^(TestFlatCollisions_UnRAR|TestFlatCollisions_SevenZip|TestUnRAR_FlatRenamesTakenNames|TestSevenZip_FlatRenamesTakenNames|TestUnRAR_CannotCreateAutoFixFlatRenamesTakenName)$

# Under OneFolder without OverwriteFiles, the external unrar and 7z keep every
# member whose flattened name collides: the tool renames a clash inside the
# stage, publish renames a name the output directory already holds, and a
# directory in the stage creates nothing (#897). TestFlatCollisions_UnRAR and
# _SevenZip need the real binaries; the argv tests pin the flags without them.

[unrar does not rename a taken name]
file internal/unpack/unrar.go
--- anchor
		args = append(args, "-o-", "-or")
--- replace
		args = append(args, "-o-")
--- end

[a degraded unrar is passed -or]
file internal/unpack/unrar.go
--- anchor
	case opts.OneFolder && !opts.HasProblem:
--- replace
	case opts.OneFolder:
--- end

[7z skips a taken name]
file internal/unpack/sevenzip.go
--- anchor
		args = append(args, "-aou")
--- replace
		args = append(args, "-aos")
--- end

[unrar's publish skips a name the output directory holds]
file internal/unpack/unrar.go
--- anchor
	published, pubErr := publishStagedExtraction(log, outDir, stageDir, opts, opts.OneFolder)
	if pubErr != nil {
--- replace
	published, pubErr := publishStagedExtraction(log, outDir, stageDir, opts, false)
	if pubErr != nil {
--- end

[unrar's auto-fix publish skips a name the output directory holds]
file internal/unpack/unrar.go
--- anchor
					published, pubErr := publishStagedExtraction(log, outDir, stageDir, opts, opts.OneFolder)
--- replace
					published, pubErr := publishStagedExtraction(log, outDir, stageDir, opts, false)
--- end

[7z's publish skips a name the output directory holds]
file internal/unpack/sevenzip.go
--- anchor
	published, pubErr := publishStagedExtraction(log, outDir, stageDir, opts, opts.OneFolder)
--- replace
	published, pubErr := publishStagedExtraction(log, outDir, stageDir, opts, false)
--- end

[publish creates the stage's directories under OneFolder]
file internal/unpack/snapshot.go
--- anchor
			if opts.OneFolder {
				return nil
			}
--- replace
			if false {
				return nil
			}
--- end
