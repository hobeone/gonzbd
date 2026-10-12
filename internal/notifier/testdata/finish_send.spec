pkg ./internal/notifier/
run ^TestFinishSend_StopsAtTheRefusedRecipient$

# finishSend stops at a refused recipient, names it, sends no DATA, and
# still QUITs the connection.

[a refused recipient is skipped]
file internal/notifier/email.go
--- anchor
			return fmt.Errorf("email: RCPT TO %s: %w", to, err)
--- replace
			_ = err
--- end

[the connection is not QUIT]
file internal/notifier/email.go
--- anchor
		_ = c.Quit()
--- replace
		_ = c.Noop()
--- end
