pkg ./internal/app/
run TestRegisterFile_SuffixesANameAlreadyTakenOnDisk$

# registerFile names a new file through jobFileLocation and then
# uniqueJobFileName, which Lstats through an os.Root on the job directory, so
# a name held by a file or a symlink gets a numeric suffix. Each half is
# reverted separately.

[registerFile skips the uniqueness check]
file internal/app/pipeline.go
--- anchor
		loc.Name = uniqueJobFileName(loc)
--- replace
		_ = uniqueJobFileName
--- end

[uniqueJobFileName never suffixes]
file internal/app/verify.go
--- anchor
	return fsutil.GetUniqueRelPath(root, loc.Name)
--- replace
	return loc.Name
--- end
