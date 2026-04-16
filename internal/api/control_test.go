package api

import (
	"net/http"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/queue"
)

func TestModePause_OK(t *testing.T) {
	t.Parallel()
	q := queue.New()
	s := testServerWithQueue(t, q)

	// Verify queue is initially not paused
	if q.IsPaused() {
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
	if !q.IsPaused() {
		t.Errorf("queue not paused after pause call")
	}
}

func TestModeResume_OK(t *testing.T) {
	t.Parallel()
	q := queue.New()
	s := testServerWithQueue(t, q)

	// Pause first
	q.PauseAll()
	if !q.IsPaused() {
		t.Fatalf("queue not paused after PauseAll")
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
	if q.IsPaused() {
		t.Errorf("queue paused after resume call")
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

func TestModeDisconnect_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=disconnect&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}

func TestModePausePP_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=pause_pp&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}

func TestModeResumePP_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=resume_pp&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}
