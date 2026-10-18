pkg ./internal/postproc/
run ^TestRepairShortfall$

[a block shortfall is reported as text, not as a ShortfallError]
file internal/postproc/stage_repair.go
--- anchor
			return fmt.Errorf("repair %q: %w", set.Name, &ShortfallError{Blocks: res.BlocksNeeded})
--- replace
			return fmt.Errorf("repair %q: %v", set.Name, &ShortfallError{Blocks: res.BlocksNeeded})
--- end

[a job with several par2 sets keeps the first set's shortfall]
file internal/postproc/stage_repair.go
--- anchor
	if len(found) != 1 && firstErr != nil {
--- replace
	if false {
--- end

[a job with one par2 set loses its shortfall]
file internal/postproc/stage_repair.go
--- anchor
	if len(found) != 1 && firstErr != nil {
--- replace
	if firstErr != nil {
--- end

[a shortfall is never recorded]
file internal/postproc/stages.go
--- anchor
		if sf, ok := errors.AsType[*ShortfallError](err); ok {
--- replace
		if sf, ok := errors.AsType[*ShortfallError](err); ok && false {
--- end

[an earlier shortfall survives a failure of another kind]
file internal/postproc/stages.go
--- anchor
		j.shortfall = 0
--- replace
		_ = j.shortfall
--- end

[a stage that did not set ParError records its error as the shortfall]
file internal/postproc/stages.go
--- anchor
	if j.ParError && !parBefore {
--- replace
	if j.ParError {
--- end

[an unknown shortfall reads as known]
file internal/postproc/stages.go
--- anchor
	return j.shortfall, j.shortfall > 0
--- replace
	return j.shortfall, true
--- end
