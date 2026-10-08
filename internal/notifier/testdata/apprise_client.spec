pkg ./internal/notifier/
run TestAppriseNotifier_(ShallowCopiesNonNilClient|DefaultTimeoutAppliedWhenClientTimeoutZero)$

[NewAppriseNotifier only copies a non-nil client when Timeout == 0]
file internal/notifier/apprise.go
--- anchor
	if client == nil {
		client = &http.Client{Timeout: defaultAppriseTimeout}
	} else {
		c := *client
		if c.Timeout == 0 {
			c.Timeout = defaultAppriseTimeout
		}
		client = &c
	}
--- replace
	if client == nil {
		client = &http.Client{Timeout: defaultAppriseTimeout}
	} else if client.Timeout == 0 {
		c := *client
		c.Timeout = defaultAppriseTimeout
		client = &c
	}
--- end

[NewAppriseNotifier leaves Timeout == 0 on a shallow-copied client]
file internal/notifier/apprise.go
--- anchor
		if c.Timeout == 0 {
			c.Timeout = defaultAppriseTimeout
		}
--- replace
		if false && c.Timeout == 0 {
			c.Timeout = defaultAppriseTimeout
		}
--- end
