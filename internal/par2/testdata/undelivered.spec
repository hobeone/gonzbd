pkg ./internal/par2/
run ^TestAssessment_Undelivered$

[undelivered inverts delivery check]
file internal/par2/assess.go
--- anchor
		if _, ok := idx.match(f.OnDisk); !ok {
			undelivered = append(undelivered, f.Desc)
		}
--- replace
		if _, ok := idx.match(f.OnDisk); ok {
			undelivered = append(undelivered, f.Desc)
		}
--- end

[undelivered skips unaccounted files]
file internal/par2/assess.go
--- anchor
	undelivered = append(undelivered, a.ID.Unaccounted...)
--- replace
--- end
