pkg ./internal/app/
run TestVerifyJobFiles_Outcomes|TestVerifyJobFiles_ReadFaultChangesNothing|TestVerifyJobFiles_FsyncErrorUntrustsTheFile|TestVerifyJobFiles_RetryDoesNotFinishOverAnIntersectionFailure|TestFinishFileByPath|TestFileCRCFromRows|TestFileFinishable|TestResolveRows|TestReadBackFile_ReadsARowLongerThanTheBuffer|TestFinishIfResolved|TestVerifyJobFiles_CancelChangesNothing|TestVerifyJobFiles_OpenErrorIsAFault|TestVerifyJobFiles_MissingDirectoryIsAFault|TestVerifyJobFiles_OpensTheResolversPath

[(a) a CRC mismatch treated as a match]
file internal/app/verify.go
--- anchor
	return crc == r.CRC32, nil
--- replace
	return true, nil
--- end

[(b) the fsync on the fresh descriptor skipped]
file internal/app/verify.go
--- anchor
	if fsyncFile(fh) != nil {
--- replace
	if false {
--- end

[(c) the maxEnd > 0 guard dropped]
file internal/app/verify.go
--- anchor
	if maxEnd > 0 && st.Size() > maxEnd {
--- replace
	if st.Size() > maxEnd {
--- end

[(d) an EIO falls through to the delete path]
file internal/app/verify.go
--- anchor
		case errors.Is(err, io.EOF):
--- replace
		case err != nil:
--- end

[(e) fileCRCFromRows accepts a gap]
file internal/app/verify.go
--- anchor
		if a < lo || a >= hi || seen[a-lo] || r.Offset != end {
--- replace
		if a < lo || a >= hi || seen[a-lo] || r.Offset < end {
--- end

[(f) the retry guard neutered]
file internal/app/verify.go
--- anchor
	if !retry {
--- replace
	if true {
--- end

[(g) a row longer than the buffer hashed from its first fill only]
file internal/app/verify.go
--- anchor
		done += int64(n)
--- replace
		done = r.Length
--- end

[(h) the shared finishable rule's policy exclusion dropped]
file internal/app/verify.go
--- anchor
	if policy != job.FetchAlways || complete {
--- replace
	if complete {
--- end

[(i) a row intersecting a kept row is not failed]
file internal/app/verify.go
--- anchor
		case intersectsAny(r, out.verified):
--- replace
		case false:
--- end

[(j) the cancellation check before finishing dropped]
file internal/app/verify.go
--- anchor
	if err := ctx.Err(); err != nil {
		return false, err
	}
--- replace
	if false {
		return false, nil
	}
--- end

[(k) a missing directory treated as a missing file]
file internal/app/verify.go
--- anchor
			return fileReadback{}, &errVerifyFault{File: dir, Err: sErr}
--- replace
			return fileReadback{deleteAll: true}, nil
--- end

[(l) the per-row cancellation check neutered]
file internal/app/verify.go
--- anchor
		if err := ctx.Err(); err != nil {
			return fileReadback{}, err
--- replace
		if false {
			return fileReadback{}, err
--- end

[(m) an open error other than ENOENT treated as absence]
file internal/app/verify.go
--- anchor
	if err != nil {
		return fileReadback{}, err
	}
	defer func() { _ = fh.Close() }()
--- replace
	if err != nil {
		return fileReadback{deleteAll: true}, nil
	}
	defer func() { _ = fh.Close() }()
--- end

[(n) the impossible-range check neutered]
file internal/app/verify.go
--- anchor
		if r.Offset < 0 || r.Length <= 0 {
--- replace
		if false {
--- end

[(o) the path derived from the name instead of the resolver]
file internal/app/verify.go
--- anchor
		path := pathFor(f.Filename)
--- replace
		path := filepath.Join(filepath.Dir(pathFor("x")), f.Filename)
--- end
