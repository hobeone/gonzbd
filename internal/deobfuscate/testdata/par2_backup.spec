pkg ./internal/deobfuscate/
run TestPar2Rename_SkipsBackupOfVerifiedTargetWithoutExclusions

[par2RenameFile does not skip par2 backup of verified target]
file internal/deobfuscate/par2.go
--- anchor
	if par2.IsBackupNamedFor(root, e.Name(), relDst, hash) {
		return Rename{}, false, nil
	}
--- replace
	if false {
		return Rename{}, false, nil
	}
--- end
