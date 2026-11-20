pkg ./internal/app/
run ^TestVerifyAndAttach_NoReaderSeesAVerifiedArticleUnfinished$

[hydration attaches first and installs the verification as a second step]
file internal/app/residency.go
--- anchor
		if installs, err = j.AttachArticles(m, vs); err != nil {
			return nil, err
		}
--- replace
		if _, err = j.AttachArticles(m, nil); err != nil {
			return nil, err
		}
		installs = make([]job.FileInstall, len(vs))
		for k, v := range vs {
			installs[k].Dropped, installs[k].Err = j.InstallFileVerification(v)
		}
--- end

[AttachArticles with verifications never attaches the manifest]
file internal/job/content.go
--- anchor
	j.progress.recompute(m)
	j.manifest = m
--- replace
	j.progress.recompute(m)
	if len(vs) == 0 {
		j.manifest = m
	}
--- end

[AttachArticles with verifications returns before attaching or installing]
file internal/job/content.go
--- anchor
	j.progress.recompute(m)
	j.manifest = m
--- replace
	j.progress.recompute(m)
	if len(vs) > 0 {
		return make([]FileInstall, len(vs)), nil
	}
	j.manifest = m
--- end
