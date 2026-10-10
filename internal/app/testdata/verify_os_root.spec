pkg ./internal/app/
run Test(ReadBackFile_RefusesASymlinkOutOfTheJobDirectory|ReadBackFile_RefusesANameThatClimbsOutOfTheJobDirectory|ReadBackFile_AMissingFileInAnExistingDirectoryIsGone|VerifyJobFiles_SymlinkOutOfTheJobDirectoryIsAFault|FinishFileByPath_RefusesASymlinkOutOfTheJobDirectory|JobFileLocation_AgreesWithTheWritersJoin)$

# The verifier opens a job's files through an os.Root on the job directory,
# so a symlink or a name leading out of it is refused at the open. Each open
# is reverted to the plain-path open it replaced, separately.

[the read-back opens by plain path]
file internal/app/verify.go
--- anchor
	fh, err := root.Open(loc.Name)
--- replace
	fh, err := os.Open(loc.Path())
--- end

[the finish opens by plain path]
file internal/app/verify.go
--- anchor
	fh, err := root.OpenFile(loc.Name, os.O_RDWR, 0)
--- replace
	fh, err := os.OpenFile(loc.Path(), os.O_RDWR, 0)
--- end

[a missing file inside the open directory is a fault]
file internal/app/verify.go
--- anchor
	if errors.Is(err, fs.ErrNotExist) {
		return fileReadback{deleteAll: true}, nil // inside a directory held open, so one that exists
--- replace
	if false {
		return fileReadback{deleteAll: true}, nil // inside a directory held open, so one that exists
--- end

[the resolver does not sanitize the name as the writer does]
file internal/app/verify.go
--- anchor
	return jobFile{Dir: jobDir, Name: fsutil.SanitizeFilename(filename, sanitize)}
--- replace
	return jobFile{Dir: jobDir, Name: fsutil.SanitizeFilename(filepath.Base(filename), sanitize)}
--- end
