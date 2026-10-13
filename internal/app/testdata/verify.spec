pkg ./internal/app/
run ^(TestVerifyJobFiles_Outcomes|TestVerifyJobFiles_ReadFaultChangesNothing|TestVerifyJobFiles_FsyncErrorUntrustsTheFile|TestVerifyJobFiles_RetryDoesNotFinishOverAnIntersectionFailure|TestFinishFileByPath|TestFinishFileByPath_FsyncsOnlyAfterATruncate|TestFileFinishable|TestResolveRows|TestResolveRows_ALongRowReachesAKeptRowAfterIt|TestReadBackFile_ReadsARowLongerThanTheBuffer|TestFinishIfResolved|TestVerifyJobFiles_CancelChangesNothing|TestVerifyJobFiles_OpenErrorIsAFault|TestVerifyJobFiles_MissingDirectoryIsAFault|TestVerifyJobFiles_OpensTheResolversPath|TestVerifyJobFiles_LeavesAFileWithNoRowsUntouched)$

[(a) a CRC mismatch treated as a match]
file internal/app/verify.go
run ^TestVerifyJobFiles_Outcomes$
--- anchor
	return crc == r.CRC32, nil
--- replace
	return true, nil
--- end

[(b) the fsync on the fresh descriptor skipped]
file internal/app/verify.go
run ^TestVerifyJobFiles_FsyncErrorUntrustsTheFile$
--- anchor
	if fsyncFile(fh) != nil {
--- replace
	if false {
--- end

[(c) the maxEnd > 0 guard dropped]
file internal/fsutil/shrink.go
run ^TestFinishFileByPath_FsyncsOnlyAfterATruncate$
--- anchor
	if end > 0 {
--- replace
	if true {
--- end

[(c2) the fsync after the truncate skipped]
file internal/fsutil/shrink.go
run ^TestFinishFileByPath_FsyncsOnlyAfterATruncate$
--- anchor
	if err := sync(f); err != nil {
		return storagefault.Classify("sync", f.Name(), err)
	}
	return nil
--- replace
	return nil
--- end

[(c4) the finish fsyncs a file it did not truncate]
file internal/fsutil/shrink.go
run ^TestFinishFileByPath_FsyncsOnlyAfterATruncate$
--- anchor
	if err != nil || !shrunk {
--- replace
	if err != nil || !shrunk && false {
--- end

[(c3) the truncate skipped]
file internal/fsutil/shrink.go
run ^TestFinishFileByPath$
--- anchor
			if err := f.Truncate(end); err != nil {
--- replace
			if err := error(nil); err != nil {
--- end

[(d) an EIO falls through to the delete path]
file internal/app/verify.go
run ^TestVerifyJobFiles_ReadFaultChangesNothing$
--- anchor
		case errors.Is(err, io.EOF):
--- replace
		case err != nil:
--- end

[(f) the retry guard neutered]
file internal/app/verify.go
run ^TestVerifyJobFiles_RetryDoesNotFinishOverAnIntersectionFailure$
--- anchor
	if !retry {
--- replace
	if true {
--- end

[(g) a row longer than the buffer hashed from its first fill only]
file internal/app/verify.go
run ^TestReadBackFile_ReadsARowLongerThanTheBuffer$
--- anchor
		done += int64(n)
--- replace
		done = r.Length
--- end

[(h) the shared finishable rule's policy exclusion dropped]
file internal/app/verify.go
run ^TestFileFinishable$
--- anchor
	if policy != job.FetchAlways || complete {
--- replace
	if complete {
--- end

[(i) a row intersecting a kept row is not failed]
file internal/app/verify.go
run ^TestResolveRows$
--- anchor
		if hit {
--- replace
		if hit && false {
--- end

[(i2) a row reaching only the kept row after it is not failed]
file internal/app/verify.go
run ^TestResolveRows_ALongRowReachesAKeptRowAfterIt$
--- anchor
			before < len(out.verified) && intersects(r, out.verified[before])
--- replace
			false
--- end

[(i3) a matching row is kept without checking the row kept before it]
file internal/app/verify.go
run ^TestResolveRows$
--- anchor
		if n := len(out.verified); n > 0 && intersects(r, out.verified[n-1]) {
--- replace
		if n := len(out.verified); n > 0 && false {
--- end

[(j) the cancellation check before finishing dropped]
file internal/app/verify.go
run ^TestFinishIfResolved$
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
run ^TestVerifyJobFiles_MissingDirectoryIsAFault$
--- anchor
		return fileReadback{}, &errVerifyFault{File: loc.Dir, Err: err}
--- replace
		return fileReadback{deleteAll: true}, nil
--- end

[(l) the per-row cancellation check neutered]
file internal/app/verify.go
run ^TestVerifyJobFiles_CancelChangesNothing$
--- anchor
		if err := ctx.Err(); err != nil {
			return fileReadback{}, err
--- replace
		if false {
			return fileReadback{}, err
--- end

[(m) an open error other than ENOENT treated as absence]
file internal/app/verify.go
run ^TestVerifyJobFiles_OpenErrorIsAFault$
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
run ^TestVerifyJobFiles_Outcomes$
--- anchor
		case !r.HasValidShape():
--- replace
		case false:
--- end

[(o) the path derived from the name instead of the resolver]
file internal/app/verify.go
run ^TestVerifyJobFiles_OpensTheResolversPath$
--- anchor
		loc, err := locate(f.Filename)
--- replace
		loc, err := locate("x")
		loc.Name = f.Filename
--- end

[(p) a file with no rows is read back]
file internal/app/verify.go
run ^TestVerifyJobFiles_LeavesAFileWithNoRowsUntouched$
--- anchor
		if len(fr) == 0 {
--- replace
		if false {
--- end
