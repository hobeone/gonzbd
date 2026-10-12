pkg ./internal/job/
run ^(TestFetchPolicyChange_PendingFollowsThePolicy|TestFetchPolicyDoors_OneResidencyRule|TestRestoreContent_RefusesADifferentFileShape)$

# #903: setFileFetchPolicy owns the pending counters a policy change moves,
# the per-article transitions count only a file the job is fetching, and both
# release doors are progress-tier writes.

[the policy writer leaves the counters alone]
file internal/job/content.go
--- anchor
	p.setPending(fi, p.pendingOf(fi))
--- replace
--- end

[pendingOf counts a file the job is not fetching]
file internal/job/progress.go
--- anchor
	if !p.countsPending(fi) {
		return 0
	}
--- replace
	if false {
		return 0
	}
--- end

[an article transition moves a held file's count]
file internal/job/progress.go
--- anchor
	if !p.countsPending(fi) {
		return
	}
--- replace
	if false {
		return
	}
--- end

[the release gates on the manifest again]
file internal/job/content.go
--- anchor
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	for _, fi := range fileIdxs {
--- replace
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	for _, fi := range fileIdxs {
--- end

[the release range check reads past the last file]
file internal/job/content.go
--- anchor
		if fi < 0 || fi >= len(j.progress.files) {
			return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fi)
--- replace
		if fi < 0 || fi > len(j.progress.files) {
			return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fi)
--- end

[the shape check ignores the per-file ranges]
file internal/job/progress.go
--- anchor
		if lo, hi := m.FileRange(fi); lo != p.fileStarts[fi] || hi != p.fileStarts[fi+1] {
--- replace
		if lo, hi := m.FileRange(fi); false && (lo != p.fileStarts[fi] || hi != p.fileStarts[fi+1]) {
--- end
