pkg ./internal/par2/
run TestIdentify_NameMatch

# Pass 1 compares the on-disk length with the packet's. Neutering the guard
# condition (rather than deleting the block, which would leave `info` unused
# and fail to compile) makes a longer name-matched file claimable again.
[the name pass skips the length check]
file internal/par2/identify.go
--- anchor
		if fd.FileSize > 0 {
			info, iErr := de.Info()
--- replace
		if false {
			info, iErr := de.Info()
--- end

# Inverted, a file of the right length is declined and the control dies.
[the name pass declines a file of the recorded length]
file internal/par2/identify.go
--- anchor
			if iErr != nil || uint64(info.Size()) != fd.FileSize { //nolint:gosec // size is non-negative
				log.Info("identify: name matched
--- replace
			if iErr != nil || uint64(info.Size()) == fd.FileSize { //nolint:gosec // size is non-negative
				log.Info("identify: name matched
--- end
