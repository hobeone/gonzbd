pkg ./internal/unpack/
run TestWithPasswords_BeforeSnapshotFailure|TestWithPasswords_AfterSnapshotFailureAborts|TestCleanupPartialFiles_RemoveFailureReturnsError|TestCleanupPartialFiles_RefusesANilSnapshot|TestRemovePartials_AVanishedPartialIsNotAFailure|TestDiscardPartialFiles_LogsACleanupFailure

# withPasswords ran a wrong-password cleanup against a nil before-snapshot
# when the pre-attempt scan of outDir failed, so every file in outDir, the
# archive volumes included, was removed as a partial of the attempt.

# The owner check neutered: a nil snapshot diffs as "every file is new".
[cleanupPartialFiles accepts a nil snapshot]
file internal/unpack/passwords.go
--- anchor
	if beforeSnap == nil {
--- replace
	if beforeSnap == nil && false {
--- end

# withPasswords' own check neutered: the owner still refuses, but the walk
# error is lost from the chain.
[the snapshot error is not returned at the cleanup]
file internal/unpack/passwords.go
--- anchor
			if snapErr != nil {
				res.Reason = FailUnknown
--- replace
			if snapErr != nil && false {
				res.Reason = FailUnknown
--- end

# The abort moved back before the attempt, failing an archive the attempt
# would have extracted.
[a failed snapshot stops an attempt that would succeed]
file internal/unpack/passwords.go
--- anchor
		beforeSnap, snapErr := snapshotDir(outDir)
--- replace
		beforeSnap, snapErr := snapshotDir(outDir)
		if snapErr != nil {
			return Result{}, snapErr
		}
--- end

# The walk error formatted rather than wrapped.
[the snapshot error is not wrapped]
file internal/unpack/passwords.go
--- anchor
				res.Err = fmt.Errorf("%s: snapshot %s before password attempt %d: %w", toolName, outDir, i+1, snapErr)
--- replace
				res.Err = fmt.Errorf("%s: snapshot %s before password attempt %d: %v", toolName, outDir, i+1, snapErr)
--- end

# A partial that is already gone counted as a removal failure.
[a vanished partial is a failure]
file internal/unpack/passwords.go
--- anchor
		} else if !errors.Is(rmErr, os.ErrNotExist) {
--- replace
		} else {
--- end

# discardPartialFiles swallows the cleanup failure.
[a cleanup failure is not logged]
file internal/unpack/passwords.go
--- anchor
	if err := cleanupPartialFiles(outDir, beforeSnap, log, toolName, 1); err != nil {
--- replace
	if err := cleanupPartialFiles(outDir, beforeSnap, log, toolName, 1); err != nil && false {
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
