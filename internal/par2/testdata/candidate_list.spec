pkg ./internal/par2/
run ^TestGoRepair_ScansOnlyTheGivenCandidates$|^TestAddCandidateFiles_

[addCandidateFiles registers the par2 directory's files instead of the list]
file internal/par2/go_par2.go
--- anchor
	for _, c := range candidates {
--- replace
	entries, _ := os.ReadDir(parDir)
	for _, e := range entries {
		c := e.Name()
--- end

[an absolute candidate is handed to par2engine without being made relative]
file internal/par2/go_par2.go
--- anchor
		if filepath.IsAbs(c) {
--- replace
		if false {
--- end
