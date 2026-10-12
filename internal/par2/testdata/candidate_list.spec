pkg ./internal/par2/
run ^TestGoRepair_ScansOnlyTheGivenCandidates$|^TestGoRepair_RelativeDownloadDir_FindsCandidates$|^TestAddCandidateFiles_

[addCandidateFiles registers the par2 directory's files instead of the list]
file internal/par2/go_par2.go
--- anchor
	for _, c := range candidates {
--- replace
	entries, _ := os.ReadDir(parDir)
	for _, e := range entries {
		c := e.Name()
--- end

[a candidate is taken relative to parDir without first being made absolute]
file internal/par2/go_par2.go
--- anchor
		abs, err := filepath.Abs(c)
--- replace
		abs, err := c, error(nil)
--- end

[parDir is used as given rather than made absolute]
file internal/par2/go_par2.go
--- anchor
	absDir, err := filepath.Abs(parDir)
--- replace
	absDir, err := parDir, error(nil)
--- end
