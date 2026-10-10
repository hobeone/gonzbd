pkg ./internal/job/
run TestResetForRetry_UncompletesAFileWithUndoneArticles

# ResetForRetry clears Complete for any file with an article that is not done,
# not only for a file whose failed articles it reset itself: a retry restores
# Complete from the retained history row of a file an earlier attempt
# finalized short, and that job never had the failed marks in memory.
[ResetForRetry clears Complete only for files whose failed articles it reset]
file internal/job/content.go
--- anchor
			case !j.progress.done.Get(i):
				unresolved = true
--- replace
			case false:
				unresolved = true
--- end

[the stale assembled CRC is kept]
file internal/job/content.go
--- anchor
			fp.Complete = false
			fp.AssembledCRC32 = 0
		}
	}
	j.progress.recompute(j.manifest)
--- replace
			fp.Complete = false
		}
	}
	j.progress.recompute(j.manifest)
--- end
