pkg ./internal/par2/
run TestGoRepair_DamagedBlockWhoseContentRecurs_IsRepairedNatively|TestGoRepair_CorruptFileWithAllBlocksPresent_IsRepairedNatively|TestGoVerify_CorruptFileWithAllBlocksPresent_IsRepairPossible|TestGoRepair_MissingFileFoundElsewhere_IsRebuilt

[the repair summary drops the files to rebuild]
file internal/par2/go_par2.go
--- anchor
		counts.UnusableDataShardCount, counts.FilesNeedingRebuild, counts.RenamesNeeded, counts.UsableParityShardCount)
--- replace
		counts.UnusableDataShardCount, 0, counts.RenamesNeeded, counts.UsableParityShardCount)
--- end

[GoVerify's Stdout drops the files to rebuild]
file internal/par2/go_par2.go
--- anchor
			counts.FilesNeedingRebuild, counts.RenamesNeeded)
--- replace
			0, counts.RenamesNeeded)
--- end
