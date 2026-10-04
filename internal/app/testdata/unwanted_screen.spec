pkg ./internal/app/
run TestScreenUnwanted$

[a stale state survives the check]
file internal/app/unwanted.go
--- anchor
	hdr.Unwanted = unwanted.StateNone
--- replace
	_ = unwanted.StateNone
--- end

[an approved job is checked anyway]
file internal/app/unwanted.go
--- anchor
	if approved {
--- replace
	if false && approved {
--- end

[a blocked job is not paused]
file internal/app/unwanted.go
--- anchor
	if err := j.SetIntent(job.IntentPause); err != nil {
--- replace
	if err := j.SetIntent(job.IntentRun); err != nil {
--- end

[the fail action does not fail]
file internal/app/unwanted.go
--- anchor
	if rules.Action() == unwanted.ActionFail {
--- replace
	if false && rules.Action() == unwanted.ActionFail {
--- end

[a rules error lets the job through]
file internal/app/unwanted.go
--- anchor
		return "", fmt.Errorf("unwanted-extension check: %w", err)
--- replace
		return "", nil
--- end
