pkg ./internal/app/
run TestResumeAtStartup_ShortenedPartialIsRefetched

[the sweep no longer clears the Complete flag it disproved]
file internal/job/content.go
--- anchor
			fp.Complete = false
			fp.AssembledCRC32 = 0
--- replace
			fp.AssembledCRC32 = 0
--- end

[the sweep no longer returns a disproved article to Outstanding]
file internal/job/content.go
--- anchor
			if j.progress.markNotDone(i) {
				fileCleared++
			}
--- replace
			if false {
				fileCleared++
			}
--- end
