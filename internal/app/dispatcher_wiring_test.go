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
	if app.Config() == nil {
		t.Fatal("app.Config() must not be nil")
	}

	w := &appWorkers{app: app}
	w.Abort("test-job")

	appNilDisp := &Application{}
	if _, ok := appNilDisp.lookupJob("test-job"); ok {
		t.Fatal("lookupJob must return false when dispatcher is nil")
	}
}
