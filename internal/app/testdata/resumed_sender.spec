pkg ./internal/app/
run ^(TestEnqueueResumedCompletion_StartDoesNotWaitForTheConsumer|TestEnqueueResumedCompletion_SenderExitsAtShutdown)$

# A Resumed completion that finds the channel full is sent from a goroutine
# that gives up on app.ctx: it neither blocks Start, whose hydrations run
# before the consumer, nor outlives the app.

[the sender never gives up]
file internal/app/record.go
--- anchor
		case <-ctx.Done():
			app.log.Info("resumed completion not delivered; the app is stopping",
--- replace
		case <-make(chan struct{}):
			_ = ctx
			app.log.Info("resumed completion not delivered; the app is stopping",
--- end

[the caller blocks on the send]
file internal/app/record.go
--- anchor
	go func() {
		defer app.resumedInFlight.Add(-1)
--- replace
	func() {
		defer app.resumedInFlight.Add(-1)
--- end
