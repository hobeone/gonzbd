pkg ./internal/postproc/
run ^(TestDeobfuscateStage_SkipsOnParError|TestDeobfuscateStage_SkipsOnUnpackError|TestRecordRepairSuccess_RecordsFileRenameOnJob)$

[deobfuscate runs despite par error]
file internal/postproc/stage_deobfuscate.go
--- anchor
	case job.ParError:
--- replace
	case false:
--- end

[deobfuscate runs despite unpack error]
file internal/postproc/stage_deobfuscate.go
--- anchor
	case job.UnpackError:
--- replace
	case false:
--- end

[recordRepairSuccess does not record par2 renames on job]
file internal/postproc/stage_repair.go
--- anchor
		for canonical, onDisk := range res.Parsed.Renames {
			job.Par2Renames[canonical] = onDisk
			job.RecordFileRename(onDisk, canonical)
			logf(ctx, log, job, slog.LevelInfo, "Par2 rename: %q → %q", onDisk, canonical)
		}
--- replace
		for canonical, onDisk := range res.Parsed.Renames {
			job.Par2Renames[canonical] = onDisk
			logf(ctx, log, job, slog.LevelInfo, "Par2 rename: %q → %q", onDisk, canonical)
		}
--- end
