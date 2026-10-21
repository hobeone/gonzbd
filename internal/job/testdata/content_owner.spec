pkg ./internal/job/
run TestAttachContent_IsTheSoleConstructorOfThePair

[AttachFiles stops deriving progress and leaves it nil]
file internal/job/content.go
--- anchor
	j.progress = newJobProgress(ft, seed)
--- replace
	j.progress = nil
--- end

[Manifest returns nil,nil instead of ErrNotResident]
file internal/job/content.go
--- anchor
		return nil, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
--- replace
		return nil, nil
--- end
