pkg ./internal/dispatch/
run TestSettleFailed_SettlesAnOpenAttemptAndRefusesANeverRunJob

# Dispatcher.SettleFailed forwards to the queue's settle, the door
# reconcileResidency settles an unreadable manifest through.

[SettleFailed settles nothing]
file internal/dispatch/tick.go
--- anchor
	if err := d.q.Settle(j, job.OutcomeFailed); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[SettleFailed settles the wrong outcome]
file internal/dispatch/tick.go
--- anchor
	if err := d.q.Settle(j, job.OutcomeFailed); err != nil {
--- replace
	if err := d.q.Settle(j, job.OutcomeOK); err != nil {
--- end

[SettleFailed swallows a refusal]
file internal/dispatch/tick.go
--- anchor
		return fmt.Errorf("dispatch: settle %s failed: %w", j.ID(), err)
--- replace
		return nil
--- end
