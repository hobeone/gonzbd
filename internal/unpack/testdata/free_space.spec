pkg ./internal/unpack/
run ^(TestDeclaredSize_MatchesWhatExtractionWrites|TestDeclaredSize_Unknown|TestFits|TestExtractors_RefuseWhatDoesNotFit|TestCheckFreeSpace_UnknownSizeProceeds|TestExtractInStage_DiskFullIsFailDiskFull|TestCheckRarVolumesFreeSpace)$

# One free-space guard replaces the archive-bomb limits (#901): before an
# extraction, the archive's declared unpacked size is compared with the free
# space above min_free_space, and an extraction that fills the disk is
# FailDiskFull whatever the engine made of it.

[fits lets an archive use the reserved space]
file internal/unpack/free_space.go
--- anchor
	return declared <= free-minFree
--- replace
	return declared <= free
--- end

[fits refuses an archive that leaves exactly min_free_space]
file internal/unpack/free_space.go
--- anchor
	return declared <= free-minFree
--- replace
	return declared < free-minFree
--- end

[extractInStage does not check free space]
file internal/unpack/snapshot.go
--- anchor
	if err := checkFreeSpace(ctx, log, archive, outDir, password, opts.MinFreeSpace); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[a refusal is not FailDiskFull]
file internal/unpack/free_space.go
--- anchor
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, ErrInsufficientSpace)
--- replace
	return errors.Is(err, syscall.ENOSPC)
--- end

[a disk-full attempt is not FailDiskFull]
file internal/unpack/free_space.go
--- anchor
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, ErrInsufficientSpace)
--- replace
	return errors.Is(err, syscall.Errno(0)) || errors.Is(err, ErrInsufficientSpace)
--- end

[a failed attempt is not classified]
file internal/unpack/snapshot.go
--- anchor
		return withDiskFull(res, err)
--- replace
		return res, err
--- end

[FileJoin does not check free space]
file internal/unpack/filejoin.go
--- anchor
	if err := checkFreeSpace(ctx, log, archive, outDir, "", opts.MinFreeSpace); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[FileJoin's failure is not classified]
file internal/unpack/filejoin.go
--- anchor
	res, err := withDiskFull(fileJoin(ctx, log.With("component", "filejoin"), archive, outDir, opts))
--- replace
	res, err := fileJoin(ctx, log.With("component", "filejoin"), archive, outDir, opts)
--- end

[an unknown size is refused]
file internal/unpack/free_space.go
--- anchor
		log.Info("unpack: no free-space check, the archive's unpacked size is not known",
			"archive", mainFile, "err", sizeErr)
		return nil
--- replace
		return ErrInsufficientSpace
--- end

[tar counts links and directories]
file internal/unpack/free_space.go
--- anchor
		if hdr.Typeflag == tar.TypeReg {
--- replace
		if true {
--- end

[tar counts only the first entry]
file internal/unpack/free_space.go
--- anchor
			n += hdr.Size
--- replace
			n = hdr.Size
--- end

[7z counts only its last member]
file internal/unpack/free_space.go
--- anchor
		n += int64(f.UncompressedSize)
--- replace
		n = int64(f.UncompressedSize)
--- end

[a split counts only its last part]
file internal/unpack/free_space.go
--- anchor
		n += st.Size()
--- replace
		n = st.Size()
--- end

[encrypted RAR headers read as a size]
file internal/unpack/free_space.go
--- anchor
	if errors.Is(err, rarheader.ErrSizeUnknown) {
--- replace
	if false {
--- end

[DirectUnpack's bound counts no volume]
file internal/unpack/free_space.go
--- anchor
	declared, err := rarDeclaredSize(volumes)
--- replace
	declared, err := rarDeclaredSize(volumes[1:])
--- end
