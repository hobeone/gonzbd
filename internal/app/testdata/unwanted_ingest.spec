pkg ./internal/app/
run TestAddJob_Unwanted|TestRetryHistoryJob_Unwanted|TestUnwantedFailMessage

[an approved job is checked anyway]
file internal/app/unwanted.go
--- anchor
	if approved {
--- replace
	if false && approved {
--- end

[a rules error lets the job through unchecked]
file internal/app/unwanted.go
--- anchor
	if err != nil {
		return "", fmt.Errorf("unwanted-extension check: %w", err)
--- replace
	if false && err != nil {
		return "", fmt.Errorf("unwanted-extension check: %w", err)
--- end

[an unreadable file list lets the job through unchecked]
file internal/app/unwanted.go
--- anchor
	m, err := j.Manifest()
	if err != nil {
--- replace
	m, err := j.Manifest()
	if err != nil && m != nil {
--- end

[action off is ignored]
file internal/app/unwanted.go
--- anchor
	if rules.Action() == unwanted.ActionOff {
--- replace
	if false && rules.Action() == unwanted.ActionOff {
--- end

[a blocked job is not marked]
file internal/app/unwanted.go
--- anchor
	hdr.Unwanted = unwanted.StateBlocked
--- replace
	hdr.Unwanted = unwanted.StateNone
--- end

[a blocked job is not paused]
file internal/app/unwanted.go
--- anchor
	if err := j.SetIntent(job.IntentPause); err != nil {
--- replace
	if err := j.SetIntent(job.IntentRun); err != nil {
--- end

[the fail action pauses instead]
file internal/app/unwanted.go
--- anchor
	if rules.Action() == unwanted.ActionFail {
--- replace
	if false && rules.Action() == unwanted.ActionFail {
--- end

[the fail message drops the count of the rest]
file internal/app/unwanted.go
--- anchor
	if more := len(names) - len(shown); more > 0 {
--- replace
	if more := len(names) - len(shown); false && more > 0 {
--- end

[AddJob never files a failed job]
file internal/app/app.go
--- anchor
	if unwantedFail != "" {
		app.maybeFinalizeJob(j, unwantedFail)
--- replace
	if false && unwantedFail != "" {
		app.maybeFinalizeJob(j, unwantedFail)
--- end

[a retry the check fails goes ahead]
file internal/app/app.go
--- anchor
	if unwantedFail != "" {
		return fmt.Errorf("app: retry %s: %s: %w", jobID, unwantedFail, ErrUnwantedRefused)
--- replace
	if false && unwantedFail != "" {
		return fmt.Errorf("app: retry %s: %s: %w", jobID, unwantedFail, ErrUnwantedRefused)
--- end

[a retry ignores allow_unwanted]
file internal/app/app.go
--- anchor
	unwantedFail, err := app.screenUnwanted(j, &hdr, allowUnwanted || entry.Unwanted == unwanted.StateApproved)
--- replace
	unwantedFail, err := app.screenUnwanted(j, &hdr, entry.Unwanted == unwanted.StateApproved)
--- end

[a retry forgets the entry's approval]
file internal/app/app.go
--- anchor
	unwantedFail, err := app.screenUnwanted(j, &hdr, allowUnwanted || entry.Unwanted == unwanted.StateApproved)
--- replace
	unwantedFail, err := app.screenUnwanted(j, &hdr, allowUnwanted || (false && entry.Unwanted == unwanted.StateApproved))
--- end

[the hand-over drops the job's unwanted state]
file internal/app/app.go
--- anchor
			Unwanted:             hdr.Unwanted,
--- replace
			Unwanted:             0,
--- end

[the history entry drops the job's unwanted state]
file internal/app/history_helper.go
--- anchor
		Unwanted:     ppJob.Unwanted,
--- replace
		Unwanted:     0,
--- end
