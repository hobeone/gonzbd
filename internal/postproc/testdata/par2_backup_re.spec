pkg ./internal/postproc/
run TestCleanupPar2Backups_ScopedToRepairedTargetsAndSkipsDirs

[existing map includes directories as valid repaired targets]
file internal/postproc/stage_repair.go
--- anchor
		if !e.IsDir() {
			existing[e.Name()] = true
		}
--- replace
		existing[e.Name()] = true
--- end

[unrepaired files are treated as repaired targets]
file internal/postproc/stage_repair.go
--- anchor
		if _, ok := repairedTargets[originalName]; !ok {
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[zero-padded split suffix accepted as par2 backup number]
file internal/postproc/stage_repair.go
--- anchor
	if suffix == "" || len(suffix) > 3 || suffix[0] < '1' || suffix[0] > '9' {
--- replace
	if suffix == "" || len(suffix) > 3 || suffix[0] < '0' || suffix[0] > '9' {
--- end

[par2Backups omitted from par2RenameExclusions]
file internal/postproc/stages.go
--- anchor
	for _, rel := range j.par2Backups {
		set[rel] = struct{}{}
	}
--- replace
	for range j.par2Backups {
	}
--- end
