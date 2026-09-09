pkg ./internal/job/
run TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing

[the empty-proof early return neutered]
file internal/job/content.go
--- anchor
	arts := proof.Articles()
	if len(arts) == 0 {
		return 0, 0, nil
	}
--- replace
	arts := proof.Articles()
	if len(arts) == 0 {
		j.contentMu.Lock()
		defer j.contentMu.Unlock()
		if j.manifest == nil || j.progress == nil {
			return 0, 0, nil
		}
		n := j.manifest.NumArticles()
		for i := range n {
			j.progress.markDone(j.manifest, i)
		}
		return 0, n, nil
	}
--- end
