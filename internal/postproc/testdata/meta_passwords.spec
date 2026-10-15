pkg ./internal/postproc/
run TestUnpackHelpers/prepareOptions_with_MetaPasswords$

[prepareOptions stops including job.MetaPasswords]
file internal/postproc/stage_unpack.go
--- anchor
	for _, pw := range job.MetaPasswords {
		if pw != "" && !slices.Contains(jobPws, pw) {
			jobPws = append(jobPws, pw)
		}
	}
--- replace
	for _, pw := range job.MetaPasswords {
		_ = pw
	}
--- end
