pkg ./internal/api/
run ^(TestQueueSetPaused_RefusalIsNotLoggedAsDone|TestQueueSetPaused_UnknownIDIsNotLogged)$

# queueSetPaused logs a refused pause or resume at Warn instead of as done,
# and passes over an unknown ID silently (#286).

[a refusal is logged as done]
file internal/api/queue.go
--- anchor
			if err != nil {
				// Logged rather than reported as done.
--- replace
			if false {
				// Logged rather than reported as done.
--- end

[an unknown ID is warned about]
file internal/api/queue.go
--- anchor
			if errors.Is(err, dispatch.ErrNotFound) {
				continue
			}
			if err != nil {
--- replace
			if false {
				continue
			}
			if err != nil {
--- end
