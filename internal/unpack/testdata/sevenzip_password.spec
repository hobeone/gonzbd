pkg ./internal/unpack/
run ^(TestGoSevenZipWithPasswords_StoredEncryptedArchiveTriesSecondPassword|TestClassify7zOutput|TestIs7zWrongPassword)$

[isSevenZipEncrypted disabled on checksum error]
file internal/unpack/go_sevenzip.go
--- anchor
			if errors.Is(err, errSevenZipChecksum) && isSevenZipEncrypted(archive.MainFile, password, f.Name) {
--- replace
			if false {
--- end

[is7zWrongPassword returns false]
file internal/unpack/passwords.go
--- anchor
	return Classify7zOutput(output) == FailWrongPassword
--- replace
	return false
--- end

[Classify7zOutput ignores cannot open encrypted archive]
file internal/unpack/classify.go
--- anchor
		strings.Contains(lower, "cannot open encrypted archive") ||
--- replace
		false ||
--- end

[Classify7zOutput ignores crc failed in encrypted file]
file internal/unpack/classify.go
--- anchor
		strings.Contains(lower, "crc failed in encrypted file") {
--- replace
		false {
--- end
