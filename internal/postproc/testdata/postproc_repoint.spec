pkg ./internal/postproc/
run TestVerifyJobCRCs_AbsentManifestErrorsRatherThanClaimingVerified|TestBuildDownloadFileList_AbsentManifestExplainsItself

[quickcheck unreadable manifest error guard neutered]
file internal/postproc/stage_quickcheck.go
--- anchor
	if _, mErr := job.Manifest(); mErr != nil {
		return fmt.Errorf("quickcheck: cannot verify CRCs without the manifest: %w", mErr)
	}
--- replace
	if false {
		return fmt.Errorf("quickcheck: cannot verify CRCs without the manifest: %w", nil)
	}
--- end

[filelist unavailable manifest guard neutered]
file internal/postproc/filelist.go
--- anchor
	m, mErr := j.Manifest()
	if mErr != nil {
		return []string{fmt.Sprintf("File listing unavailable: %v", mErr)}
	}
--- replace
	m, mErr := j.Manifest()
	if false {
		return []string{fmt.Sprintf("File listing unavailable: %v", mErr)}
	}
--- end
