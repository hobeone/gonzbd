pkg ./internal/job/
run TestRecoveryVolumeBlocks$|TestIsRecoveryVolume$

[the "-" form read as a count]
file internal/job/par2name.go
--- anchor
	if err != nil || n > MaxVolumeBlocks {
		return 0, false
	}
	return n, true
--- replace
	if err != nil || n > MaxVolumeBlocks {
		return 0, true
	}
	return n, true
--- end

[a count above the PAR2 per-volume maximum is accepted]
file internal/job/par2name.go
--- anchor
	if err != nil || n > MaxVolumeBlocks {
--- replace
	if err != nil || false && n > MaxVolumeBlocks {
--- end

[the per-volume maximum itself is refused]
file internal/job/par2name.go
--- anchor
	if err != nil || n > MaxVolumeBlocks {
--- replace
	if err != nil || n >= MaxVolumeBlocks {
--- end

[a non-volume name reported as one]
file internal/job/par2name.go
--- anchor
	if m == nil {
		return 0, false
	}
--- replace
	if m == nil {
		return 0, true
	}
--- end

[the count off by one]
file internal/job/par2name.go
--- anchor
	return n, true
}
--- replace
	return n + 1, true
}
--- end
