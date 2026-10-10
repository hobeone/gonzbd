pkg ./internal/app/
run Test(ReadBackFile_RefusesASymlinkOutOfTheJobDirectory|ReadBackFile_RefusesANameThatClimbsOutOfTheJobDirectory|ReadBackFile_AMissingFileInAnExistingDirectoryIsGone|VerifyJobFiles_SymlinkOutOfTheJobDirectoryIsAFault|FinishFileByPath_RefusesASymlinkOutOfTheJobDirectory|JobFileLocation_AgreesWithTheWritersJoin|VerifyJobFiles_LinkToASiblingIsAFault|FinishFileByPath_RefusesALinkToASibling|VerifierOpens_CloseTheirRoots)$

[the read-back leaves its root open]
file internal/app/verify.go
--- anchor
	defer func() { _ = root.Close() }() // a directory handle; nothing to lose on close
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDONLY, 0)
--- replace
	defer func() { _ = root }()
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDONLY, 0)
--- end

[the finish leaves its root open]
file internal/app/verify.go
--- anchor
	defer func() { _ = root.Close() }() // a directory handle; nothing to lose on close
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDWR, 0)
--- replace
	defer func() { _ = root }()
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDWR, 0)
--- end

# The verifier opens a job's files with fsutil.OpenNoFollow on an os.Root on
# the job directory, so a name leading out of it, or a symlink in the file's
# place wherever it points, is refused at the open. Each open is reverted,
# separately, to the plain-path open it replaced and to a rooted open that
# follows in-directory links.

[the read-back opens by plain path]
file internal/app/verify.go
--- anchor
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDONLY, 0)
--- replace
	fh, err := os.Open(loc.Path())
--- end

[the finish opens by plain path]
file internal/app/verify.go
--- anchor
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDWR, 0)
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

[the read-back follows a link inside the job directory]
file internal/app/verify.go
--- anchor
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDONLY, 0)
--- replace
	fh, err := root.Open(loc.Name)
--- end

[the finish follows a link inside the job directory]
file internal/app/verify.go
--- anchor
	fh, err := fsutil.OpenNoFollow(root, loc.Name, os.O_RDWR, 0)
--- replace
	fh, err := root.OpenFile(loc.Name, os.O_RDWR, 0)
--- end

[the opens accept a hard link to a sibling]
file internal/fsutil/nofollow_unix.go
--- anchor
	if st.Nlink != 1 {
--- replace
	if false {
--- end
