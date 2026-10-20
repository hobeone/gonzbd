pkg ./internal/job/
run ^(TestRestoreContent_RefusesASameCountDifferentSplit|TestFetchPolicyChange_PendingFollowsThePolicy|TestFetchPolicyDoors_OneResidencyRule|TestAttachArticles_RefusesAManifestWithADifferentFileShape)$

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
file internal/job/release.go
--- anchor
	if j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
--- replace
	if j.manifest == nil || j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
--- end

[the forwarder's shape comparison is dropped, so a same-count different split is rebuilt over the job's table]
file internal/job/content.go
--- anchor
		if !slices.Equal(ft.Specs(), m.Files().Specs()) {
--- replace
		if !slices.Equal(ft.Specs(), m.Files().Specs()) && false {
--- end

[the pairing check is dropped, so a manifest of another table attaches]
file internal/job/content.go
--- anchor
	if m.Files() != j.fileTable {
--- replace
	if false {
--- end
