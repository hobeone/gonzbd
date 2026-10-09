pkg ./internal/app/
run TestRecorder_UntrustPurgesPendingRows

[the purge dropped]
file internal/app/record.go
--- anchor
		r.purgeLocked(j, fv)
--- replace
		_ = fv
--- end

[the dirty-entry half of the purge dropped]
file internal/app/record.go
--- anchor
		delete(r.dirty[j], fv.FileIdx)
--- replace
		_ = fv
--- end
