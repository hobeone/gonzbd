pkg ./internal/unpack/
run TestWithPasswords_BeforeSnapshotFailurePreservesVolumes|TestWithPasswords_AfterSnapshotFailureAborts|TestCleanupPartialFiles_RemoveFailureReturnsError

# withPasswords ran a wrong-password cleanup against a nil before-snapshot
# when the pre-attempt scan of outDir failed, so every file in outDir, the
# archive volumes included, was removed as a partial of the attempt.

# The before-snapshot guard neutered: the shape the loop had before.
[an attempt runs without a before-snapshot]
file internal/unpack/passwords.go
--- anchor
		if snapErr != nil {
			err := fmt.Errorf("%s: snapshot %s before password attempt %d: %w", toolName, outDir, i+1, snapErr)
--- replace
		if snapErr != nil && false {
			err := fmt.Errorf("%s: snapshot %s before password attempt %d: %w", toolName, outDir, i+1, snapErr)
--- end

# A cleanup that could not finish no longer stops the loop.
[the loop continues past a failed cleanup]
file internal/unpack/passwords.go
--- anchor
			if cleanErr := cleanupPartialFiles(outDir, beforeSnap, log, toolName, i+1); cleanErr != nil {
--- replace
			if cleanErr := cleanupPartialFiles(outDir, beforeSnap, log, toolName, i+1); cleanErr != nil && false {
--- end

# The after-attempt scan failure swallowed, as before.
[a failed after-scan reports success]
file internal/unpack/passwords.go
--- anchor
		return fmt.Errorf("snapshot %s after attempt: %w", outDir, err)
--- replace
		return nil
--- end

# A failed unlink swallowed, as before.
[a failed remove is not reported]
file internal/unpack/passwords.go
--- anchor
			errs = append(errs, rmErr)
--- replace
			_ = rmErr
--- end

# A failed unlink stops the remaining removals. The fixture's removable file
# sorts after the stuck one, and cleanupPartialFiles sorts what it removes.
[the first failed remove ends the sweep]
file internal/unpack/passwords.go
--- anchor
			errs = append(errs, rmErr)
--- replace
			return rmErr
--- end
