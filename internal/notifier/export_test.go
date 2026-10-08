package notifier

import "net/http"

// FormatMessageForTest exports formatMessage for white-box testing in notifier_test.
func (e *EmailNotifier) FormatMessageForTest(ev Event) []byte {
	return e.formatMessage(ev)
}

// DefaultAppriseTimeout exports defaultAppriseTimeout for testing.
const DefaultAppriseTimeout = defaultAppriseTimeout

// ClientForTest returns the configured HTTP client on AppriseNotifier for testing.
func (a *AppriseNotifier) ClientForTest() *http.Client {
	return a.client
}
