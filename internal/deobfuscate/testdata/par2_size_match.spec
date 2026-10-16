pkg ./internal/deobfuscate/
run ^TestPar2Rename_

[par2RenameFile does not check file size match]
file internal/deobfuscate/par2.go
--- anchor
	if desc.FileSize > 0 {
		info, err := e.Info()
		if err != nil {
			return Rename{}, false, nil //nolint:nilerr // unreadable entry skips rename
		}
		if info.Size() < 0 || uint64(info.Size()) != desc.FileSize { //nolint:gosec // size is non-negative
			return Rename{}, false, nil
		}
	}
--- replace
	if false {
		return Rename{}, false, nil
	}
--- end

[buildHashToNameMap does not mark conflicting FileName descriptors ambiguous]
file internal/deobfuscate/par2.go
--- anchor
			if existing, ok := hashToDesc[d.Hash16k]; ok && (existing.FileName != d.FileName || existing.FileSize != d.FileSize) {
--- replace
			if existing, ok := hashToDesc[d.Hash16k]; ok && existing.FileSize != d.FileSize {
--- end

[buildHashToNameMap does not mark conflicting FileSize descriptors ambiguous]
file internal/deobfuscate/par2.go
--- anchor
			if existing, ok := hashToDesc[d.Hash16k]; ok && (existing.FileName != d.FileName || existing.FileSize != d.FileSize) {
--- replace
			if existing, ok := hashToDesc[d.Hash16k]; ok && existing.FileName != d.FileName {
--- end

