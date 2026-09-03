package app

import "testing"

// TestApplicationConstructsAWiredDispatcher pins that app.New produces a
// dispatcher with both ports satisfied. dispatch.New panics on a nil Residency
// or Runner, so this test failing to panic IS the assertion.
func TestApplicationConstructsAWiredDispatcher(t *testing.T) {
	app := newTestApplication(t)
	if app.Dispatcher() == nil {
		t.Fatal("app.New must construct a Dispatcher")
	}
}
