pkg ./internal/par2/
run ^(TestCollectManifests_SkipsASetIDAlreadyParsed|TestCollectManifests_PartialDuplicateKeepsTheOtherCopysEntries|TestCollectManifests_PrefersTheCopyWithChecksums|TestIdentification_SliceSize)$

# Copies of one par2 set (equal SetID and slice size) are merged by FileID.

[a set parsed twice under two names is collected twice]
file internal/par2/fsops.go
--- anchor
			case !dup:
--- replace
			case !dup || i >= 0:
--- end

[a later copy is skipped whole, so a damaged first copy shadows it]
file internal/par2/fsops.go
--- anchor
		if seen {
			log.Info("par2 set already parsed under another name; merging its entries",
--- replace
		if seen {
			continue
			log.Info("par2 set already parsed under another name; merging its entries",
--- end

[a description without a CRC is never replaced]
file internal/par2/fsops.go
--- anchor
			case c.entries[i].FileCRC32 == 0 && fd.FileCRC32 != 0:
--- replace
			case false:
--- end

[the last copy read always wins]
file internal/par2/fsops.go
--- anchor
			case c.entries[i].FileCRC32 == 0 && fd.FileCRC32 != 0:
--- replace
			case true:
--- end

[an entry carries the name of the copy it was read from]
file internal/par2/fsops.go
--- anchor
			fd.Set = name
--- replace
			fd.Set = set.Name
--- end

[the set key drops the slice size]
file internal/par2/fsops.go
--- anchor
		info := parsedSetInfo{id: parsed.SetID, sliceSize: parsed.SliceSize}
--- replace
		info := parsedSetInfo{id: parsed.SetID}
--- end
