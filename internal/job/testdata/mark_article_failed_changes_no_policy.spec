pkg ./internal/job/
run ^(TestMarkArticleFailed_ChangesNoFetchPolicy|TestMarkArticleFailedCharge_ChangesNoFetchPolicy)$

# A permanent article failure changes no fetch policy and records no release
# reason, whatever the job's Policy.Repair: only the Assessing verdict
# releases, sized from the written rows (app.maybeReleaseRecoveryVolumes).

[a failure releases the volumes of a repairing job]
file internal/job/content.go
--- anchor
	j.progress.markFailedUncovered(j.manifest, artIdx, charge)
	return nil
--- replace
	j.progress.markFailedUncovered(j.manifest, artIdx, charge)
	if j.policy.Repair {
		j.undeferRecovery(j.progress.DeferredRecoveryIndices())
	}
	return nil
--- end

[a failure releases the volumes of any job]
file internal/job/content.go
--- anchor
	j.progress.markFailedUncovered(j.manifest, artIdx, charge)
	return nil
--- replace
	j.progress.markFailedUncovered(j.manifest, artIdx, charge)
	j.undeferRecovery(j.progress.DeferredRecoveryIndices())
	return nil
--- end

[a failure records a release reason without releasing]
file internal/job/content.go
--- anchor
	j.progress.markFailedUncovered(j.manifest, artIdx, charge)
	return nil
--- replace
	j.progress.markFailedUncovered(j.manifest, artIdx, charge)
	j.progress.setPar2ReleaseReason("permanent article failure")
	return nil
--- end
