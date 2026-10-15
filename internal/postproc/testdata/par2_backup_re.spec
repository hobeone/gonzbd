pkg ./internal/postproc/
run TestCleanupPar2Backups_ScopedToRepairedTargetsAndSkipsDirs|TestRepairStage_ExternalPar2RecordsBackupAndCrashRerunCleansIt|TestFindVerifiedPar2Backups|TestSharesProbeChunk

[existing map includes directories as valid repaired targets]
file internal/postproc/stage_repair.go
--- anchor
		if !e.IsDir() {
			existing[e.Name()] = true
		}
--- replace
		existing[e.Name()] = true
--- end

[pre-existing .N files before repair are treated as newly created par2 backups]
file internal/postproc/stage_repair.go
--- anchor
		if _, existedBefore := beforeRepair[name]; existedBefore {
			continue
		}
--- replace
		if false {
			continue
		}
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

[Par2CleanupStage omits hash-verified par2 backup discovery on crash rerun]
file internal/postproc/stage_par2cleanup.go
--- anchor
	candidates = append(candidates, findVerifiedPar2Backups(job.DownloadDir, sets, job.Sanitize, s.ParseOpts, log)...)
--- replace
	_ = findVerifiedPar2Backups
--- end

[findVerifiedPar2Backups does not skip par2 members named X.N]
file internal/postproc/stage_par2cleanup.go
--- anchor
		if _, isTarget := knownTargets[name]; isTarget {
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[findVerifiedPar2Backups skips backup size upper bound]
file internal/postproc/stage_par2cleanup.go
--- anchor
		if ierr != nil || info.Size() > spec.size {
--- replace
		if ierr != nil {
--- end

[findVerifiedPar2Backups skips probe chunk content verification]
file internal/postproc/stage_par2cleanup.go
--- anchor
		if sharesProbeChunk(root, name, relDst, info.Size()) {
--- replace
		if sharesProbeChunk(root, name, relDst, info.Size()) || true {
--- end

[sharesProbeChunk accepts all-zero probe chunks]
file internal/postproc/stage_par2cleanup.go
--- anchor
		if !slices.ContainsFunc(candBuf[:n], func(b byte) bool { return b != 0 }) {
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[sharesProbeChunk omits probe 0 at start of file]
file internal/postproc/stage_par2cleanup.go
--- anchor
	offsets := [4]int64{
		0,
		par2BackupSampleChunk,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
--- replace
	offsets := [4]int64{
		-1,
		par2BackupSampleChunk,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
--- end

[sharesProbeChunk omits probe 1 at 16 KiB]
file internal/postproc/stage_par2cleanup.go
--- anchor
	offsets := [4]int64{
		0,
		par2BackupSampleChunk,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
--- replace
	offsets := [4]int64{
		0,
		-1,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
--- end

[sharesProbeChunk omits midpoint probe]
file internal/postproc/stage_par2cleanup.go
--- anchor
	offsets := [4]int64{
		0,
		par2BackupSampleChunk,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
--- replace
	offsets := [4]int64{
		0,
		par2BackupSampleChunk,
		-1,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
--- end

[sharesProbeChunk omits tail probe]
file internal/postproc/stage_par2cleanup.go
--- anchor
	offsets := [4]int64{
		0,
		par2BackupSampleChunk,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
--- replace
	offsets := [4]int64{
		0,
		par2BackupSampleChunk,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		-1,
	}
--- end
