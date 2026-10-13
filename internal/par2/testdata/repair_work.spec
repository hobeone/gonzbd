pkg ./internal/par2/
run TestGoRepair_DamagedBlockWhoseContentRecurs_IsRepairedNatively|TestGoRepair_CorruptFileWithAllBlocksPresent_IsRepairedNatively|TestGoVerify_CorruptFileWithAllBlocksPresent_IsRepairPossible|TestGoRepair_MissingFileFoundElsewhere_IsRebuilt|TestGoVerify_RenameOnly_ReportsTheRename|TestGoRepair_ParityAndRebuildInOneSet

[the repair summary drops the files to rebuild]
file internal/par2/go_par2.go
--- anchor
		counts.UnusableDataShardCount, counts.FilesNeedingRebuild, counts.RenamesNeeded, counts.UsableParityShardCount)
--- replace
		counts.UnusableDataShardCount, 0, counts.RenamesNeeded, counts.UsableParityShardCount)
--- end

[the repair summary drops the files to rename]
file internal/par2/go_par2.go
--- anchor
		counts.UnusableDataShardCount, counts.FilesNeedingRebuild, counts.RenamesNeeded, counts.UsableParityShardCount)
--- replace
		counts.UnusableDataShardCount, counts.FilesNeedingRebuild, 0, counts.UsableParityShardCount)
--- end

[the repair summary drops the missing blocks]
file internal/par2/go_par2.go
--- anchor
		counts.UnusableDataShardCount, counts.FilesNeedingRebuild, counts.RenamesNeeded, counts.UsableParityShardCount)
--- replace
		0, counts.FilesNeedingRebuild, counts.RenamesNeeded, counts.UsableParityShardCount)
--- end

[GoVerify's Stdout drops the files to rebuild]
file internal/par2/go_par2.go
--- anchor
			counts.FilesNeedingRebuild, counts.RenamesNeeded)
--- replace
			0, counts.RenamesNeeded)
--- end

[GoVerify's Stdout drops the files to rename]
file internal/par2/go_par2.go
--- anchor
			counts.FilesNeedingRebuild, counts.RenamesNeeded)
--- replace
			counts.FilesNeedingRebuild, 0)
--- end
