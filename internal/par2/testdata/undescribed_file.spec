pkg ./internal/par2/
run ^(TestIdentify_DeclaredButUndescribedIsUnaccounted|TestRecoveryFileIDs)$

# A file a set's Main packet lists in its recovery set, but that no copy of the
# set describes, is Unaccounted.

[the Main packet's recovery set is not recorded]
file internal/par2/parser.go
--- anchor
		set.RecoveryFileIDs = recoveryFileIDs(body)
--- replace
		set.RecoveryFileIDs = nil
--- end

[a count the body cannot hold is read anyway]
file internal/par2/parser.go
--- anchor
	if uint64(n) > uint64(len(body)-12)/16 {
--- replace
	if false {
--- end

[the non-recovery IDs are read as the recovery set]
file internal/par2/parser.go
--- anchor
	n := binary.LittleEndian.Uint32(body[8:12])
--- replace
	n := uint32((len(body) - 12) / 16)
--- end

[a described file is not excluded]
file internal/par2/fsops.go
--- anchor
		if _, ok := at[k]; ok {
--- replace
		if false {
--- end

[a file two copies declare is counted twice]
file internal/par2/fsops.go
--- anchor
			if !declared[k] {
--- replace
			if true {
--- end

[an undescribed file is dropped when another file is described]
file internal/par2/identify.go
--- anchor
	for _, fd := range c.undescribed {
--- replace
	for _, fd := range c.undescribed[:0] {
--- end

[an undescribed file is dropped when nothing is described]
file internal/par2/identify.go
--- anchor
		return Identification{Unaccounted: c.undescribed, sets: infos}, nil
--- replace
		return Identification{sets: infos}, nil
--- end
