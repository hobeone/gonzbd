pkg ./internal/par2/
run ^(TestGoVerify_BlocksNeeded|TestGoVerify_BlocksNeeded_IsNetOfRecoveryOnDisk|TestCandidateFiles)$

[BlocksNeeded counts the unusable data blocks without netting the recovery on disk]
file internal/par2/go_par2.go
--- anchor
		res.BlocksNeeded = counts.BlocksNeeded()
		res.DataBlocks
--- replace
		res.BlocksNeeded = counts.UnusableDataShardCount
		res.DataBlocks
--- end

[DataBlocks leaves out the unusable data blocks]
file internal/par2/go_par2.go
--- anchor
		res.DataBlocks = counts.UsableDataShardCount + counts.UnusableDataShardCount
--- replace
		res.DataBlocks = counts.UsableDataShardCount
--- end

[CandidateFiles lists the par2 files too]
file internal/par2/go_par2.go
--- anchor
		if e.IsDir() || strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
--- replace
		if e.IsDir() {
--- end

[CandidateFiles lists directories too]
file internal/par2/go_par2.go
--- anchor
		if e.IsDir() || strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
--- replace
		if strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
--- end
