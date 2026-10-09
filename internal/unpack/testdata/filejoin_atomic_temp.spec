pkg ./internal/unpack/
run TestFileJoin_|TestSyncAndPublishJoin_

[FileJoin writes directly to outRel instead of RootedCreateTempPerm]
file internal/unpack/filejoin.go
--- anchor
	outFile, tmpRel, err := fsutil.RootedCreateTempPerm(ctx, root, outRel, 0o666)
--- replace
	outFile, err := fsutil.RootedOpenFile(root, outRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	tmpRel := outRel
--- end

[FileJoin creates temp file with 0o600 instead of 0o666]
file internal/unpack/filejoin.go
--- anchor
	outFile, tmpRel, err := fsutil.RootedCreateTempPerm(ctx, root, outRel, 0o666)
--- replace
	outFile, tmpRel, err := fsutil.RootedCreateTemp(ctx, root, outRel)
--- end

[FileJoin skips Lstat existing-output guard]
file internal/unpack/filejoin.go
--- anchor
	if info, err := root.Lstat(outRel); err == nil && info.Mode().IsRegular() {
--- replace
	if info, err := root.Lstat(outRel); false && err == nil && info.Mode().IsRegular() {
--- end

[FileJoin treats non-regular file at outRel as existing output]
file internal/unpack/filejoin.go
--- anchor
	if info, err := root.Lstat(outRel); err == nil && info.Mode().IsRegular() {
--- replace
	if info, err := root.Lstat(outRel); err == nil && (true || info.Mode().IsRegular()) {
--- end

[FileJoin checks sortedNumericParts before Lstat existing-output guard]
file internal/unpack/filejoin.go
--- anchor
	if info, err := root.Lstat(outRel); err == nil && info.Mode().IsRegular() {
		log.Info("filejoin: output already exists, skipping join", "outPath", outPath)
		return Result{ExtractedFiles: []string{outPath}}, nil
	}

	// Validate contiguity after checking whether outRel already exists, so a
	// crash during archive cleanup after .001 was already removed does not fail
	// the rerun of an already-completed join.
	parts, err := sortedNumericParts(archive.Parts)
	if err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: %w", err)
	}
--- replace
	parts, err := sortedNumericParts(archive.Parts)
	if err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: %w", err)
	}
	if info, err := root.Lstat(outRel); err == nil && info.Mode().IsRegular() {
		log.Info("filejoin: output already exists, skipping join", "outPath", outPath)
		return Result{ExtractedFiles: []string{outPath}}, nil
	}
--- end

[FileJoin skips deferred temp file removal on failure]
file internal/unpack/filejoin.go
--- anchor
		if !published {
--- replace
		if false && !published {
--- end

[syncAndPublishJoin skips outFile.Sync before Rename]
file internal/unpack/filejoin.go
--- anchor
	if err := outFile.Sync(); err != nil {
		return fmt.Errorf("filejoin: sync output: %w", err)
	}
--- replace
	if err := error(nil); err != nil {
		return fmt.Errorf("filejoin: sync output: %w", err)
	}
--- end

[syncAndPublishJoin skips outFile.Close before Rename]
file internal/unpack/filejoin.go
--- anchor
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("filejoin: close output: %w", err)
	}
--- replace
	if err := error(nil); err != nil {
		return fmt.Errorf("filejoin: close output: %w", err)
	}
--- end

[syncAndPublishJoin skips publishing rename to outRel]
file internal/unpack/filejoin.go
--- anchor
	if err := root.Rename(tmpRel, outRel); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end
