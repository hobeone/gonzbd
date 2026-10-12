pkg ./internal/par2/
run TestGoRepair_CorruptFileWithAllBlocksPresent_IsNotSuccess|TestGoVerify_CorruptFileWithAllBlocksPresent_IsNotAllFilesOK|TestGoRepair_MissingFileFoundElsewhere_DoesNotReportSuccess|TestGoRepair_HealthyData|TestProtectedFilesIntact_HashMatches|TestProtectedFilesIntact_HashMismatch|TestProtectedFilesIntact_EngineVerifiedIsNotReread|TestProtectedFilesIntact_Missing|TestProtectedFilesIntact_SymlinkIsMissing|TestProtectedFilesIntact_Unreadable|TestProtectedFilesIntact_IndexUnreadable_Refuses|TestVerdictRecorder_RecordsEngineOKReport|TestVerdictRecorder_IgnoresOtherRecordsAndPassesThrough

[GoRepair ignores the intactness verdict]
file internal/par2/go_par2.go
--- anchor
		succeed := func(engineVerdict string) {
			if problem := protectedFilesIntact(parfile, verified); problem != "" {
--- replace
		succeed := func(engineVerdict string) {
			if problem := protectedFilesIntact(parfile, verified); false && problem != "" {
--- end

[GoVerify ignores the intactness verdict]
file internal/par2/go_par2.go
--- anchor
		case !counts.RepairNeeded():
			if problem := protectedFilesIntact(parfile, verified); problem != "" {
--- replace
		case !counts.RepairNeeded():
			if problem := protectedFilesIntact(parfile, verified); false && problem != "" {
--- end

[full-file hash comparison neutered]
file internal/par2/go_par2.go
--- anchor
	if [16]byte(h.Sum(nil)) != fd.FullHash {
--- replace
	if false {
--- end

[every protected file trusted as engine-verified]
file internal/par2/go_par2.go
--- anchor
	if verified.has(fd.FileName) {
--- replace
	if true {
--- end

[engine-verified files re-hashed anyway]
file internal/par2/go_par2.go
--- anchor
	if verified.has(fd.FileName) {
--- replace
	if false {
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

[an unreadable file passes]
file internal/par2/go_par2.go
--- anchor
	f, err := root.Open(name)
	if err != nil {
		return fmt.Sprintf("unreadable: %v", err)
--- replace
	f, err := root.Open(name)
	if err != nil {
		return ""
--- end

[an unparseable index passes]
file internal/par2/go_par2.go
--- anchor
		return fmt.Sprintf("the par2 index could not be re-read to confirm it: %v", err)
--- replace
		return ""
--- end

[engine OK report not recorded]
file internal/par2/go_par2.go
--- anchor
	if r.Message == engineFileOKMessage {
--- replace
	if false {
--- end

[records other than OK recorded as verified]
file internal/par2/go_par2.go
--- anchor
	if r.Message == engineFileOKMessage {
--- replace
	if true {
--- end

[wrapped handler's level ignored]
file internal/par2/go_par2.go
--- anchor
	if !h.Handler.Enabled(ctx, r.Level) {
--- replace
	if false {
--- end
