pkg ./internal/notifier/
run ^TestSetConnDeadline$

# The SMTP connection takes the context's deadline, or smtpOperationTimeout
# from now when the context has none.

[the context's deadline is ignored]
file internal/notifier/email.go
--- anchor
	if dl, ok := ctx.Deadline(); ok {
--- replace
	if dl, ok := ctx.Deadline(); false && ok {
--- end

[the fallback deadline is not applied]
file internal/notifier/email.go
--- anchor
		_ = conn.SetDeadline(time.Now().Add(smtpOperationTimeout))
--- replace
		_ = conn.SetDeadline(time.Now())
--- end
