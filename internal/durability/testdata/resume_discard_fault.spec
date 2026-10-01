pkg ./internal/durability/
run TestDiscard_NamesTheStoreAndOp|TestResume_SurfacesADiscardFailure|TestResume_ShortFileDiscardFailureNamesTheStore|TestResume_MissingFileDiscardAbandonedByItsCallerIsNotAFault|TestResume_ShortFileDiscardAbandonedByItsCallerIsNotAFault

[discard's failed delete is swallowed instead of being classified as a fault]
file internal/durability/resume.go
--- anchor
		return storagefault.Classify("delete", r.runs.Path(), werr)
--- replace
		return storagefault.Classify("delete", r.runs.Path(), nil)
--- end

[discard's failed delete drops the store's path]
file internal/durability/resume.go
--- anchor
		return storagefault.Classify("delete", r.runs.Path(), werr)
--- replace
		return storagefault.Classify("delete", "", werr)
--- end

[discard's failed delete is not checked for a caller that stopped waiting]
file internal/durability/resume.go
--- anchor
	if err := r.runs.deleteFile(ctx, jobID, fileIdx); err != nil {
		werr := storeFailure(ctx, err)
--- replace
	if err := r.runs.deleteFile(ctx, jobID, fileIdx); err != nil {
		werr := err
--- end

[discard's ctx-ended carve-out is dropped, so a cancelled caller is classified as storage]
file internal/durability/resume.go
--- anchor
		if errors.Is(werr, ErrTargetUnavailable) {
			return fmt.Errorf("durability: resume discard runs job=%s file=%d: %w", jobID, fileIdx, werr)
		}
--- replace
		if false {
			return fmt.Errorf("durability: resume discard runs job=%s file=%d: %w", jobID, fileIdx, werr)
		}
--- end
