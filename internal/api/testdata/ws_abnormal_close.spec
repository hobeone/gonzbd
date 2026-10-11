pkg ./internal/api/
run TestWebSocketLifecycleLogging_AbnormalClose

[the recorded reason no longer classifies the disconnect]
file internal/api/events.go
--- anchor
		c.closeReason = reason
--- replace
		c.closeReason = reason + " (unclassified)"
--- end
