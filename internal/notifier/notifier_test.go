// Package notifier_test covers event routing, Apprise HTTP dispatch, script
// execution, and email message formatting.
//
// Email dial/TLS paths are not tested here — they require a live SMTP server.
// The formatMessage helper is tested directly as a white-box unit test.
package notifier_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	. "github.com/hobeone/sabnzbd-go/internal/notifier"
)

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ----- EventType.String -----

func TestEventTypeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		et   EventType
		want string
	}{
		{DownloadStarted, "DownloadStarted"},
		{DownloadComplete, "DownloadComplete"},
		{DownloadFailed, "DownloadFailed"},
		{PostProcessingComplete, "PostProcessingComplete"},
		{PostProcessingFailed, "PostProcessingFailed"},
		{DiskFull, "DiskFull"},
		{QueueDone, "QueueDone"},
		{Warning, "Warning"},
		{Error, "Error"},
		{EventType(9999), "EventType(9999)"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.et.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ----- Dispatcher routing -----

type fakeNotifier struct {
	mu        sync.Mutex
	name      string
	mask      []EventType
	received  []Event
	failOn    EventType
	sendDelay time.Duration
}

func (f *fakeNotifier) Name() string { return f.name }
func (f *fakeNotifier) Accepts(t EventType) bool {
	for _, m := range f.mask {
		if m == t {
			return true
		}
	}
	return false
}
func (f *fakeNotifier) Send(ctx context.Context, e Event) error {
	if f.sendDelay > 0 {
		select {
		case <-time.After(f.sendDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn == e.Type {
		return fmt.Errorf("fake failure on %s", e.Type)
	}
	f.received = append(f.received, e)
	return nil
}
func (f *fakeNotifier) events() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.received))
	copy(out, f.received)
	return out
}

func TestDispatcherRouting(t *testing.T) {
	t.Parallel()
	logger := newTestLogger(t)
	d := NewDispatcher(logger)

	nA := &fakeNotifier{name: "A", mask: []EventType{DownloadComplete, DownloadFailed}}
	nB := &fakeNotifier{name: "B", mask: []EventType{QueueDone}}
	d.Register(nA)
	d.Register(nB)

	ctx := context.Background()
	evComplete := Event{Type: DownloadComplete, Title: "done", Body: "ok", Timestamp: time.Now()}
	evQueue := Event{Type: QueueDone, Title: "queue", Body: "empty", Timestamp: time.Now()}

	d.Dispatch(ctx, evComplete)
	d.Dispatch(ctx, evQueue)

	if got := nA.events(); len(got) != 1 || got[0].Type != DownloadComplete {
		t.Errorf("nA got %v, want [DownloadComplete]", got)
	}
	if got := nB.events(); len(got) != 1 || got[0].Type != QueueDone {
		t.Errorf("nB got %v, want [QueueDone]", got)
	}
}

func TestDispatcherFailureDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	logger := newTestLogger(t)
	d := NewDispatcher(logger)

	nFail := &fakeNotifier{name: "fail", mask: []EventType{DownloadComplete}, failOn: DownloadComplete}
	nOK := &fakeNotifier{name: "ok", mask: []EventType{DownloadComplete}}
	d.Register(nFail)
	d.Register(nOK)

	d.Dispatch(context.Background(), Event{Type: DownloadComplete, Timestamp: time.Now()})

	if got := nOK.events(); len(got) != 1 {
		t.Errorf("nOK should still receive event despite nFail failing, got %v", got)
	}
}

// ----- Apprise -----

func TestAppriseTypeMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		et       EventType
		wantType string
	}{
		{DownloadComplete, "success"},
		{PostProcessingComplete, "success"},
		{QueueDone, "success"},
		{DownloadFailed, "failure"},
		{PostProcessingFailed, "failure"},
		{Error, "failure"},
		{DiskFull, "warning"},
		{Warning, "warning"},
		{DownloadStarted, "info"},
	}
	for _, tc := range cases {
		t.Run(tc.et.String(), func(t *testing.T) {
			t.Parallel()
			var received map[string]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body) //nolint:errcheck // test helper
				_ = json.Unmarshal(body, &received)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			n := NewAppriseNotifier(AppriseConfig{
				URL:        srv.URL,
				ServiceURL: "ntfy://topic",
				EventMask:  []EventType{tc.et},
			}, srv.Client())

			ev := Event{Type: tc.et, Title: "t", Body: "b", Timestamp: time.Now()}
			if err := n.Send(context.Background(), ev); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got := received["type"]; got != tc.wantType {
				t.Errorf("type = %q, want %q", got, tc.wantType)
			}
		})
	}
}

func TestAppriseJSONShape(t *testing.T) {
	t.Parallel()
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body) //nolint:errcheck // test helper
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := NewAppriseNotifier(AppriseConfig{
		URL:        srv.URL,
		ServiceURL: "slack://token",
		EventMask:  []EventType{DownloadComplete},
	}, srv.Client())

	ev := Event{Type: DownloadComplete, Title: "My Title", Body: "My Body", Timestamp: time.Now()}
	if err := n.Send(context.Background(), ev); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if body["urls"] != "slack://token" {
		t.Errorf("urls = %q", body["urls"])
	}
	if body["title"] != "My Title" {
		t.Errorf("title = %q", body["title"])
	}
	if body["body"] != "My Body" {
		t.Errorf("body = %q", body["body"])
	}
	if body["type"] != "success" {
		t.Errorf("type = %q", body["type"])
	}
}

func TestAppriseNonOKStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := NewAppriseNotifier(AppriseConfig{
		URL:       srv.URL,
		EventMask: []EventType{Warning},
	}, srv.Client())

	err := n.Send(context.Background(), Event{Type: Warning, Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

// ----- Script -----

func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatalf("writeScript: %v", err)
	}
	return p
}

func TestScriptSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := writeScript(t, dir, "ok.sh", `echo "$1 $2 $3"`)

	n := NewScriptNotifier(ScriptConfig{
		Path:      p,
		Timeout:   5 * time.Second,
		EventMask: []EventType{DownloadComplete},
	})
	ev := Event{Type: DownloadComplete, Title: "title", Body: "body", Timestamp: time.Now()}
	if err := n.Send(context.Background(), ev); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestScriptNonZeroExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := writeScript(t, dir, "fail.sh", `echo "oops" >&2; exit 1`)

	n := NewScriptNotifier(ScriptConfig{
		Path:      p,
		Timeout:   5 * time.Second,
		EventMask: []EventType{DownloadFailed},
	})
	err := n.Send(context.Background(), Event{Type: DownloadFailed, Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for exit 1")
	}
	if !strings.Contains(err.Error(), "oops") {
		t.Errorf("error should contain stderr; got: %v", err)
	}
}

func TestScriptTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := writeScript(t, dir, "slow.sh", `sleep 10`)

	n := NewScriptNotifier(ScriptConfig{
		Path:      p,
		Timeout:   100 * time.Millisecond,
		EventMask: []EventType{QueueDone},
	})
	start := time.Now()
	err := n.Send(context.Background(), Event{Type: QueueDone, Timestamp: time.Now()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Allow up to 5s: 100ms timeout + 2s WaitDelay + scheduling slack.
	// The script sleeps 10s, so if we return before 5s the kill path fired.
	if elapsed > 5*time.Second {
		t.Errorf("timeout not enforced: elapsed %v", elapsed)
	}
}

// ----- Email message formatting -----

func TestEmailFormatMessage(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	n := NewEmailNotifier(EmailConfig{
		From: "sab@example.com",
		To:   []string{"user@example.com", "admin@example.com"},
	})
	ev := Event{
		Type:      DownloadComplete,
		Title:     "My Download",
		Body:      "Finished successfully.",
		Timestamp: ts,
	}
	msg := n.FormatMessage(ev)
	s := string(msg)

	checks := []struct {
		label string
		want  string
	}{
		{"From header", "From: sab@example.com"},
		{"To header", "To: user@example.com, admin@example.com"},
		{"Subject header", "Subject: SABnzbd: My Download"},
		{"body text", "Finished successfully."},
	}
	for _, c := range checks {
		if !strings.Contains(s, c.want) {
			t.Errorf("%s: want %q in message:\n%s", c.label, c.want, s)
		}
	}
}

func TestEmailDialRespectsContext(t *testing.T) {
	t.Parallel()
	// Use a non-routable IP to simulate a dial timeout (TEST-NET-1).
	n := NewEmailNotifier(EmailConfig{
		Host:      "192.0.2.1",
		Port:      25,
		From:      "test@example.com",
		To:        []string{"user@example.com"},
		EventMask: []EventType{Warning},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := n.Send(ctx, Event{Type: Warning, Timestamp: time.Now()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("send blocked for %v, expected ~100ms", elapsed)
	}
}

func TestDispatchConcurrent(t *testing.T) {
	t.Parallel()
	logger := newTestLogger(t)
	d := NewDispatcher(logger)

	n1 := &fakeNotifier{
		name:      "delay100ms",
		mask:      []EventType{DownloadComplete},
		sendDelay: 100 * time.Millisecond,
	}
	n2 := &fakeNotifier{
		name:      "delay200ms",
		mask:      []EventType{DownloadComplete},
		sendDelay: 200 * time.Millisecond,
	}
	d.Register(n1)
	d.Register(n2)

	ctx := context.Background()
	start := time.Now()
	d.Dispatch(ctx, Event{Type: DownloadComplete, Timestamp: time.Now()})
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Errorf("dispatch blocked for sum of delays: %v", elapsed)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("dispatch returned before all finished: %v", elapsed)
	}

	if len(n1.events()) != 1 || len(n2.events()) != 1 {
		t.Errorf("expected both to receive event")
	}
}
