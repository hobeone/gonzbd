pkg ./internal/app/
run ^TestMaybeReleaseRecoveryVolumes_VolumeAsIndex$

# No par2 on disk: the first release is one volume as the index; once a
# volume has been released and still no set parses, every volume is released.

[the already-released test is neutered to always true]
file internal/app/app.go
--- anchor
			case fetched == 0:
--- replace
			case true || fetched == 0:
--- end

[the already-released test is neutered to always false]
file internal/app/app.go
--- anchor
			case fetched == 0:
--- replace
			case false && fetched == 0:
--- end

[the index volume release is unsized]
file internal/app/app.go
--- anchor
				sizing = job.NeedBlocks(1)
--- replace
				sizing = job.Unsized()
--- end
