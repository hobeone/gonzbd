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
