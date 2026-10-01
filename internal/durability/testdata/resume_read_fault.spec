pkg ./internal/durability/
run TestResume_RunReadFailureNamesTheStore|TestResume_RunReadAbandonedByItsCallerIsNotAFault

[Resume's failed run read is swallowed instead of being classified as a fault]
file internal/durability/resume.go
--- anchor
		return ResumeResult{}, storagefault.Classify("read", r.runs.Path(), werr)
--- replace
		return ResumeResult{}, storagefault.Classify("read", r.runs.Path(), nil)
--- end

[Resume's failed run read names the download file instead of the store]
file internal/durability/resume.go
--- anchor
		return ResumeResult{}, storagefault.Classify("read", r.runs.Path(), werr)
--- replace
		return ResumeResult{}, storagefault.Classify("read", path, werr)
--- end

[Resume's failed run read is not checked for a caller that stopped waiting]
file internal/durability/resume.go
--- anchor
		werr := storeFailure(ctx, err)
--- replace
		werr := err
--- end

[Resume's ctx-ended carve-out is dropped, so a cancelled caller is classified as storage]
file internal/durability/resume.go
--- anchor
		if errors.Is(werr, ErrTargetUnavailable) {
			return ResumeResult{}, fmt.Errorf("durability: resume runs job=%s file=%d: %w", jobID, fileIdx, werr)
		}
--- replace
		if false {
			return ResumeResult{}, fmt.Errorf("durability: resume runs job=%s file=%d: %w", jobID, fileIdx, werr)
		}
--- end
