pkg ./internal/app/
run ^TestDirectUnpackOptsFromConfig$

# downloads.min_free_space reaches DirectUnpack's options.

[min_free_space does not reach DirectUnpack]
file internal/app/directunpack_orchestrator.go
--- anchor
		MinFreeSpace:     int64(snap.Downloads.MinFreeSpace),
--- replace
		MinFreeSpace:     0,
--- end
