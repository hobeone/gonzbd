pkg ./internal/rarheader/
run ^(TestUnpackedSize|TestUnpackedSize_NotDeclared|TestUnpackedSize_RAR3Volumes)$

# UnpackedSize sums what a RAR set's file headers declare, counting each
# member once however many volumes it spans and directories not at all, and
# reports ErrSizeUnknown where the headers are encrypted (#901).

[RAR5 counts a continued member in every volume]
file internal/rarheader/unpacked_size.go
--- anchor
			if hdr.flags&rar5FlagDataNotFirst == 0 {
--- replace
			if true {
--- end

[RAR5 counts only the last member]
file internal/rarheader/unpacked_size.go
--- anchor
				total += size
--- replace
				total = size
--- end

[RAR5 encrypted headers are walked]
file internal/rarheader/unpacked_size.go
--- anchor
		case rar5TypeEncryption:
			return 0, ErrSizeUnknown
--- replace
		case rar5TypeEncryption:
--- end

[RAR3 counts a continued member in every volume]
file internal/rarheader/unpacked_size.go
--- anchor
			if typ == rar3TypeFile && flags&rar3FileSplitBefore == 0 && flags&rar3FileDirMask != rar3FileDirMask {
--- replace
			if typ == rar3TypeFile && flags&rar3FileDirMask != rar3FileDirMask {
--- end

[RAR3 counts directories]
file internal/rarheader/unpacked_size.go
--- anchor
			if typ == rar3TypeFile && flags&rar3FileSplitBefore == 0 && flags&rar3FileDirMask != rar3FileDirMask {
--- replace
			if typ == rar3TypeFile && flags&rar3FileSplitBefore == 0 {
--- end

[RAR3 encrypted headers are walked]
file internal/rarheader/unpacked_size.go
--- anchor
			if flags&rar3MainPassword != 0 {
--- replace
			if false {
--- end
