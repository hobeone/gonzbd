pkg ./internal/par2/
run TestIdentify_NameMatch|TestAssess_NameMatch

# Pass 1 claims a name match whatever its length and marks a wrong length;
# verifyIdentified counts a marked file Unverified (#837).

# The shape this replaced: declining the match leaves a one-entry set with
# nothing identified, so the job holds its recovery volumes.
[the name pass declines a file of a different length]
file internal/par2/identify.go
--- anchor
		claimedFile[name] = true
		claimedEntry[ei] = true
		id.Files = append(id.Files, Identified{OnDisk: name, Desc: fd, By: by, lengthDiffers: lengthDiffers})
--- replace
		if lengthDiffers {
			return
		}
		claimedFile[name] = true
		claimedEntry[ei] = true
		id.Files = append(id.Files, Identified{OnDisk: name, Desc: fd, By: by, lengthDiffers: lengthDiffers})
--- end

# Only a longer file marked: the shorter subtests die.
[a shorter file is not marked]
file internal/par2/identify.go
--- anchor
			lengthDiffers = iErr != nil || uint64(info.Size()) != fd.FileSize //nolint:gosec // size is non-negative
--- replace
			lengthDiffers = iErr != nil || uint64(info.Size()) > fd.FileSize //nolint:gosec // size is non-negative
--- end

# Only a shorter file marked: the stale-tail subtests die.
[a longer file is not marked]
file internal/par2/identify.go
--- anchor
			lengthDiffers = iErr != nil || uint64(info.Size()) != fd.FileSize //nolint:gosec // size is non-negative
--- replace
			lengthDiffers = iErr != nil || uint64(info.Size()) < fd.FileSize //nolint:gosec // size is non-negative
--- end

# Inverted, a file of the recorded length is marked and the control dies.
[a file of the recorded length is marked]
file internal/par2/identify.go
--- anchor
			lengthDiffers = iErr != nil || uint64(info.Size()) != fd.FileSize //nolint:gosec // size is non-negative
--- replace
			lengthDiffers = iErr != nil || uint64(info.Size()) == fd.FileSize //nolint:gosec // size is non-negative
--- end

# A packet recording no length (0) is compared anyway.
[the recorded-length guard is dropped]
file internal/par2/identify.go
--- anchor
		if fd.FileSize > 0 {
			info, iErr := de.Info()
--- replace
		if true {
			info, iErr := de.Info()
--- end

# The mark is set but verification ignores it, so the stale tail verifies on
# its rows' CRC.
[verification ignores the mark]
file internal/par2/assess.go
--- anchor
		case f.lengthDiffers:
--- replace
		case false:
--- end
