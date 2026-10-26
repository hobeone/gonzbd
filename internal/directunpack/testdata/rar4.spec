pkg ./internal/directunpack/
run TestDirectUnpack_RAR4StoredLegacySet|TestDirectUnpack_SkipsCompressedRAR4|TestDirectUnpack_RAR5RefusalIsAFailure|TestSettleLocked

# DirectUnpack extracts a stored RAR 1.5-4.x set, and records a RAR 1.5-4.x set
# rarengine refuses as a format it does not decode as skipped, not failed. The
# same refusal from a RAR5 set stays a failure, and an aborted set is aborted.

[RAR 1.5-4.x skipped by signature again]
file internal/directunpack/directunpack.go
--- anchor
		if err != nil {
			return fmt.Errorf("%w: %w", errNotRAR, err)
		}
--- replace
		if err != nil || ver != 5 {
			return fmt.Errorf("%w: %w", errNotRAR, err)
		}
--- end

[a RAR 1.5-4.x refusal is a failure]
file internal/directunpack/directunpack.go
--- anchor
	return errors.Is(err, errNotRAR) || errors.Is(err, errRAR4Undecodable)
--- replace
	return errors.Is(err, errNotRAR)
--- end

[any format refusal is a skip, RAR5 included]
file internal/directunpack/directunpack.go
--- anchor
				if ver == 3 && errors.Is(extractErr, rarengine.ErrUnsupportedFormat) {
--- replace
				if errors.Is(extractErr, rarengine.ErrUnsupportedFormat) {
--- end

[a skip outranks Abort]
file internal/directunpack/directunpack.go
--- anchor
	case d.killed:
		d.failedSets[setname] = FailedSet{Reason: "aborted"}
--- replace
	case d.killed && !leftToUnpack(err):
		d.failedSets[setname] = FailedSet{Reason: "aborted"}
--- end
