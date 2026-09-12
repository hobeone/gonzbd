pkg ./internal/job/
run TestResetForRetry

# ResetForRetry used to downgrade FetchNever to FetchIfNeeded, a third writer
# that existed only to repair the second one (RestoreFileMeta's unguarded
# overwrite). With the overwrite gone the repair has nothing to repair, and
# keeping it would silently re-enable on-demand par2 for a job whose owner had
# turned the feature off between attempts (#329).
#
# This mutation lives in the internal/job spec, not the internal/app one,
# because a spec runs `go test <pkg>` and only an internal/job test can kill
# it. After the ownership fix nothing on the retry path is ever FetchNever, so
# restoring the branch is a no-op there -- an app-side spec would report
# SURVIVED while proving nothing.
#
# Anchored on the anyReset block plus the loop close: "Complete = false" alone
# appears twice in this file, and an ambiguous anchor is refused rather than
# run.
[ResetForRetry downgrades a discarded volume back to held]
file internal/job/content.go
--- anchor
		if anyReset {
			j.progress.files[fi].Complete = false
		}
	}
--- replace
		if anyReset {
			j.progress.files[fi].Complete = false
		}
		if j.progress.files[fi].Fetch == FetchNever {
			j.progress.files[fi].Fetch = FetchIfNeeded
		}
	}
--- end
