package notifier

// FormatMessageForTest exports formatMessage for white-box testing in notifier_test.
func (e *EmailNotifier) FormatMessageForTest(ev Event) []byte {
	return e.formatMessage(ev)
}
