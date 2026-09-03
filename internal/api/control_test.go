package api

import (
	"net/http"
	"testing"
)

func TestModePause_OK(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	// Verify queue is initially not paused
	if d.Paused() {
		t.Fatalf("queue initially paused")
	}

	rr := apiGet(t, s.Handler(), "/api?mode=pause&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	// Verify queue is now paused
	if !d.Paused() {
		t.Errorf("queue not paused after pause call")
	}
}

func TestModeResume_OK(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	// Pause first
	d.Pause()
	if !d.Paused() {
		t.Fatalf("queue not paused after Pause")
	}

	rr := apiGet(t, s.Handler(), "/api?mode=resume&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	// Verify queue is now not paused
	if d.Paused() {
		t.Errorf("queue paused after resume call")
	}
}

func TestModeResume_WithApp(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=resume&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
}

func TestModeShutdown_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=shutdown&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != false {
		t.Errorf("status = %v; want false", m["status"])
	}
}

func TestModeRestart_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=restart&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}

func TestModeDisconnect_OK(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=disconnect&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}
}

func TestModePausePP_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=pause_pp&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != false {
		t.Errorf("status = %v; want false", m["status"])
	}
}

func TestModeResumePP_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=resume_pp&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != false {
		t.Errorf("status = %v; want false", m["status"])
	}
}
