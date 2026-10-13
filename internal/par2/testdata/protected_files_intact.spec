pkg ./internal/par2/
run TestGoRepair_SymlinkAtProtectedName_IsNotSuccess|TestGoVerify_SymlinkAtProtectedName_IsNotAllFilesOK|TestGoRepair_HealthyData|TestProtectedFilesIntact_RegularFilePassesWithoutHashing|TestProtectedFilesIntact_Missing|TestProtectedFilesIntact_SymlinkIsMissing|TestProtectedFilesIntact_IndexUnreadable_Refuses

[GoRepair ignores the intactness verdict]
file internal/par2/go_par2.go
--- anchor
		succeed := func(engineVerdict string) {
			if problem := protectedFilesIntact(parfile); problem != "" {
--- replace
		succeed := func(engineVerdict string) {
			if problem := protectedFilesIntact(parfile); false && problem != "" {
--- end

[GoVerify ignores the intactness verdict]
file internal/par2/go_par2.go
--- anchor
		case !counts.RepairNeeded():
			if problem := protectedFilesIntact(parfile); problem != "" {
--- replace
		case !counts.RepairNeeded():
			if problem := protectedFilesIntact(parfile); false && problem != "" {
--- end

[missing-file check neutered]
file internal/par2/go_par2.go
--- anchor
		if err != nil || !info.Mode().IsRegular() {
--- replace
		if false && (err != nil || !info.Mode().IsRegular()) {
--- end

[a symlink at the protected name accepted]
file internal/par2/go_par2.go
--- anchor
		if err != nil || !info.Mode().IsRegular() {
--- replace
		if err != nil || (false && !info.Mode().IsRegular()) {
--- end

[Stat follows the link instead of Lstat]
file internal/par2/go_par2.go
--- anchor
		info, err := root.Lstat(filepath.FromSlash(fd.FileName))
--- replace
		info, err := root.Stat(filepath.FromSlash(fd.FileName))
--- end

[a problem found is not reported]
file internal/par2/go_par2.go
--- anchor
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("%d protected file(s) are not intact
--- replace
	if true {
		return ""
	}
	return fmt.Sprintf("%d protected file(s) are not intact
--- end

[an unparseable index passes]
file internal/par2/go_par2.go
--- anchor
		return fmt.Sprintf("the par2 index could not be re-read to confirm it: %v", err)
--- replace
		return ""
--- end
