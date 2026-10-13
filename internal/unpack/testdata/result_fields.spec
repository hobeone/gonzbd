pkg ./internal/unpack/
run ^(TestExtractors_ResultNamesEngineAndRelativeFiles|TestUnRARWithPasswords_PreVerifiedWrongPasswordsRunNoUnrar|TestUnRARWithPasswords_AllPreVerifiedWrong)$

# Each extractor writes its own Result.Engine on every return, and every
# Result lists ExtractedFiles relative to outDir (#911). UnRAR refuses a
# password the archive's check value proves wrong without running unrar, so
# withPasswords no longer synthesises a Result of its own. The unrar and 7z
# arms need the real binaries.

[GoUnRAR does not name itself]
file internal/unpack/go_unrar.go
--- anchor
	res.Engine = "go_unrar"
--- replace
	_ = res
--- end

[UnRAR does not name itself]
file internal/unpack/unrar.go
--- anchor
	res.Engine = "unrar"
--- replace
	_ = res
--- end

[GoSevenZip does not name itself]
file internal/unpack/go_sevenzip.go
--- anchor
	res.Engine = "go_7z"
--- replace
	_ = res
--- end

[SevenZip does not name itself]
file internal/unpack/sevenzip.go
--- anchor
	res.Engine = "7z"
--- replace
	_ = res
--- end

[GoTar does not name itself]
file internal/unpack/go_tar.go
--- anchor
	res.Engine = "go_tar"
--- replace
	_ = res
--- end

[FileJoin does not name itself]
file internal/unpack/filejoin.go
--- anchor
	res.Engine = "filejoin"
--- replace
	_ = res
--- end

[FileJoin lists its output by absolute path]
file internal/unpack/filejoin.go
--- anchor
	log.Info("filejoin: join complete", "outPath", outPath, "parts", len(parts))
	return Result{ExtractedFiles: []string{outRel}}, nil
--- replace
	log.Info("filejoin: join complete", "outPath", outPath, "parts", len(parts))
	return Result{ExtractedFiles: []string{outPath}}, nil
--- end

[a staged extraction does not list what it published]
file internal/unpack/snapshot.go
--- anchor
	res.ExtractedFiles = published
--- replace
	_ = published
--- end

[UnRAR runs unrar with a password the check value proves wrong]
file internal/unpack/unrar.go
--- anchor
	if password != "" && rarPreVerify(archive, password) {
--- replace
	if false {
--- end
