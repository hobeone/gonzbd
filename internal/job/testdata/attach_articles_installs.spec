pkg ./internal/job/
run ^(TestAttachArticles_(InstallsEachVerification|RefusedInstallsNothing)|TestInstallFileVerificationLocked_RefusesWithoutChange)$

[the install proceeds on a job that is not resident]
file internal/job/verification.go
--- anchor
	if j.manifest == nil || j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
--- replace
	if j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
--- end

[the attach installs nothing]
file internal/job/content.go
--- anchor
		out[k].Dropped, out[k].Err = j.installFileVerificationLocked(v)
--- replace
		_, _ = k, v
--- end

[the install stops at the first file it cannot install]
file internal/job/content.go
--- anchor
		out[k].Dropped, out[k].Err = j.installFileVerificationLocked(v)
--- replace
		out[k].Dropped, out[k].Err = j.installFileVerificationLocked(v)
		if out[k].Err != nil {
			return out[:k+1], nil
		}
--- end
