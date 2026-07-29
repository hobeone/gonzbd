// Package notifier_test covers event routing, Apprise HTTP dispatch, script
// execution, and email message formatting.
//
// Email dial/TLS paths are not tested here — they require a live SMTP server.
// The FormatMessage helper is tested directly as a white-box unit test.
package notifier_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	. "github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/testutil"
)

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.DiscardHandler)
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
	return slices.Contains(f.mask, t)
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

	ctx := t.Context()
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

	d.Dispatch(t.Context(), Event{Type: DownloadComplete, Timestamp: time.Now()})

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
			if err := n.Send(t.Context(), ev); err != nil {
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
	if err := n.Send(t.Context(), ev); err != nil {
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

	err := n.Send(t.Context(), Event{Type: Warning, Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

// ----- Script -----

// writeScript writes an executable shell script into dir and returns its
// path. Tests here run in parallel and exec these scripts, so the write
// goes through testutil.WriteExecutable to avoid the ETXTBSY flake
// described there (issue #239).
func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	return testutil.WriteExecutable(t, filepath.Join(dir, name), "#!/bin/sh\n"+content)
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
	if err := n.Send(t.Context(), ev); err != nil {
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
	err := n.Send(t.Context(), Event{Type: DownloadFailed, Timestamp: time.Now()})
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
	err := n.Send(t.Context(), Event{Type: QueueDone, Timestamp: time.Now()})
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
	msg := n.FormatMessageForTest(ev)
	s := string(msg)

	checks := []struct {
		label string
		want  string
	}{
		{"From header", "From: sab@example.com"},
		{"To header", "To: user@example.com, admin@example.com"},
		{"Subject header", "Subject: GoNZBD: My Download"},
		{"MIME-Version", "MIME-Version: 1.0"},
		{"Content-Type", "Content-Type: text/plain; charset=utf-8"},
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

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := n.Send(ctx, Event{Type: Warning, Timestamp: time.Now()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	// Under -race/high load, TCP dial cancellation and teardown regularly takes > 1s.
	if elapsed > 5*time.Second {
		t.Fatalf("send blocked for %v, expected ~100ms", elapsed)
	}
}

func TestDispatchConcurrent(t *testing.T) {
	t.Parallel()
	logger := newTestLogger(t)
	d := NewDispatcher(logger)

	n1 := &fakeNotifier{
		name:      "delay200ms",
		mask:      []EventType{DownloadComplete},
		sendDelay: 200 * time.Millisecond,
	}
	n2 := &fakeNotifier{
		name:      "delay300ms",
		mask:      []EventType{DownloadComplete},
		sendDelay: 300 * time.Millisecond,
	}
	d.Register(n1)
	d.Register(n2)

	ctx := t.Context()
	start := time.Now()
	d.Dispatch(ctx, Event{Type: DownloadComplete, Timestamp: time.Now()})
	elapsed := time.Since(start)

	// Concurrency check: n1 takes 200ms, n2 takes 300ms.
	// Concurrent execution completes in max(200ms, 300ms) = ~300ms.
	// Serial execution takes 200ms + 300ms = 500ms minimum.
	// Ceiling of 450ms allows 150ms of -race scheduling slack while detecting serial regression.
	if elapsed >= 450*time.Millisecond {
		t.Errorf("dispatch serialized notifiers: elapsed %v (expected < 450ms)", elapsed)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("dispatch returned before all finished: elapsed %v", elapsed)
	}

	if len(n1.events()) != 1 || len(n2.events()) != 1 {
		t.Errorf("expected both to receive event")
	}
}

// ----- Mock SMTP Server and Email Notifier Tests -----

func TestEmailNotifier_Send_Success(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		read := func() string {
			buf := make([]byte, 1024)
			n, _ := conn.Read(buf)
			return string(buf[:n])
		}

		write("220 mock-smtp.example.com GoNZBD SMTP\r\n")
		_ = read() // EHLO
		write("250-mock-smtp.example.com\r\n250 AUTH PLAIN\r\n")
		_ = read() // AUTH PLAIN
		write("235 Authentication successful\r\n")
		_ = read() // MAIL FROM
		write("250 OK\r\n")
		_ = read() // RCPT TO
		write("250 OK\r\n")
		_ = read() // DATA
		write("354 Start mail input\r\n")
		_ = read() // body data
		write("250 OK\r\n")
		_ = read() // QUIT
		write("221 Goodbye\r\n")
	}()

	n := NewEmailNotifier(EmailConfig{
		Host:      host,
		Port:      port,
		Username:  "user",
		Password:  "pass",
		From:      "from@example.com",
		To:        []string{"to@example.com"},
		EventMask: []EventType{DownloadComplete},
	})

	err = n.Send(t.Context(), Event{
		Type:      DownloadComplete,
		Title:     "Success Title",
		Body:      "Success Body",
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
}

func TestEmailNotifier_Send_Auth_Failure(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		read := func() string {
			buf := make([]byte, 1024)
			n, _ := conn.Read(buf)
			return string(buf[:n])
		}

		write("220 mock-smtp.example.com GoNZBD SMTP\r\n")
		_ = read() // EHLO
		write("250-mock-smtp.example.com\r\n250 AUTH PLAIN\r\n")
		_ = read() // AUTH PLAIN
		write("535 Authentication credentials invalid\r\n")
	}()

	n := NewEmailNotifier(EmailConfig{
		Host:      host,
		Port:      port,
		Username:  "user",
		Password:  "pass",
		From:      "from@example.com",
		To:        []string{"to@example.com"},
		EventMask: []EventType{DownloadComplete},
	})

	err = n.Send(t.Context(), Event{
		Type:      DownloadComplete,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
}

func TestEmailNotifier_Send_Mail_Failure(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		read := func() string {
			buf := make([]byte, 1024)
			n, _ := conn.Read(buf)
			return string(buf[:n])
		}

		write("220 mock-smtp.example.com GoNZBD SMTP\r\n")
		_ = read() // EHLO
		write("250-mock-smtp.example.com\r\n250 AUTH PLAIN\r\n")
		_ = read() // AUTH PLAIN
		write("235 Authentication successful\r\n")
		_ = read() // MAIL FROM
		write("550 Sender rejected\r\n")
	}()

	n := NewEmailNotifier(EmailConfig{
		Host:      host,
		Port:      port,
		Username:  "user",
		Password:  "pass",
		From:      "from@example.com",
		To:        []string{"to@example.com"},
		EventMask: []EventType{DownloadComplete},
	})

	err = n.Send(t.Context(), Event{
		Type:      DownloadComplete,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("expected MAIL FROM error, got nil")
	}
}

func TestEmailNotifier_Send_Rcpt_Failure(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		read := func() string {
			buf := make([]byte, 1024)
			n, _ := conn.Read(buf)
			return string(buf[:n])
		}

		write("220 mock-smtp.example.com GoNZBD SMTP\r\n")
		_ = read() // EHLO
		write("250-mock-smtp.example.com\r\n250 AUTH PLAIN\r\n")
		_ = read() // AUTH PLAIN
		write("235 Authentication successful\r\n")
		_ = read() // MAIL FROM
		write("250 OK\r\n")
		_ = read() // RCPT TO
		write("550 User unknown\r\n")
	}()

	n := NewEmailNotifier(EmailConfig{
		Host:      host,
		Port:      port,
		Username:  "user",
		Password:  "pass",
		From:      "from@example.com",
		To:        []string{"to@example.com"},
		EventMask: []EventType{DownloadComplete},
	})

	err = n.Send(t.Context(), Event{
		Type:      DownloadComplete,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("expected RCPT TO error, got nil")
	}
}

func TestEmailNotifier_Send_Data_Failure(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		read := func() string {
			buf := make([]byte, 1024)
			n, _ := conn.Read(buf)
			return string(buf[:n])
		}

		write("220 mock-smtp.example.com GoNZBD SMTP\r\n")
		_ = read() // EHLO
		write("250-mock-smtp.example.com\r\n250 AUTH PLAIN\r\n")
		_ = read() // AUTH PLAIN
		write("235 Authentication successful\r\n")
		_ = read() // MAIL FROM
		write("250 OK\r\n")
		_ = read() // RCPT TO
		write("250 OK\r\n")
		_ = read() // DATA
		write("554 Failure\r\n")
	}()

	n := NewEmailNotifier(EmailConfig{
		Host:      host,
		Port:      port,
		Username:  "user",
		Password:  "pass",
		From:      "from@example.com",
		To:        []string{"to@example.com"},
		EventMask: []EventType{DownloadComplete},
	})

	err = n.Send(t.Context(), Event{
		Type:      DownloadComplete,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("expected DATA error, got nil")
	}
}

func TestEmailNotifier_Send_STARTTLS_Failure(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		read := func() string {
			buf := make([]byte, 1024)
			n, _ := conn.Read(buf)
			return string(buf[:n])
		}

		write("220 mock-smtp.example.com GoNZBD SMTP\r\n")
		_ = read() // EHLO
		write("250-mock-smtp.example.com\r\n250 STARTTLS\r\n")
		_ = read() // STARTTLS
		write("220 Ready to start TLS\r\n")
		// The client will start TLS handshake, server just closes connection
	}()

	n := NewEmailNotifier(EmailConfig{
		Host:      host,
		Port:      port,
		From:      "from@example.com",
		To:        []string{"to@example.com"},
		UseTLS:    true,
		EventMask: []EventType{DownloadComplete},
	})

	err = n.Send(t.Context(), Event{
		Type:      DownloadComplete,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("expected STARTTLS handshake error, got nil")
	}
	if !strings.Contains(err.Error(), "starttls") {
		t.Fatalf("expected starttls error, got: %v", err)
	}
}

func TestEmailNotifier_Send_ImplicitTLS_DialFailure(t *testing.T) {
	t.Parallel()
	n := NewEmailNotifier(EmailConfig{
		Host:      "127.0.0.1",
		Port:      1, // closed port, instantly refuses connection
		From:      "from@example.com",
		To:        []string{"to@example.com"},
		UseSSL:    true,
		EventMask: []EventType{DownloadComplete},
	})

	err := n.Send(t.Context(), Event{
		Type:      DownloadComplete,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	if !strings.Contains(err.Error(), "tls dial") {
		t.Fatalf("expected tls dial error, got: %v", err)
	}
}

// ----- Additional Script and Apprise Tests -----

func TestScriptNotifier_TimeoutZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := writeScript(t, dir, "ok.sh", `echo "ok"`)

	n := NewScriptNotifier(ScriptConfig{
		Path:      p,
		Timeout:   0, // triggers default timeout of 30s
		EventMask: []EventType{DownloadComplete},
	})
	err := n.Send(t.Context(), Event{Type: DownloadComplete, Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("expected success with default timeout, got: %v", err)
	}
}

func TestAppriseNotifier_DefaultClient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Pass nil client to trigger DefaultClient path
	n := NewAppriseNotifier(AppriseConfig{
		URL:       srv.URL,
		EventMask: []EventType{Warning},
	}, nil)

	err := n.Send(t.Context(), Event{Type: Warning, Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("expected success with default client, got: %v", err)
	}
}

func TestAppriseStatus300(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultipleChoices) // 300
	}))
	defer srv.Close()

	n := NewAppriseNotifier(AppriseConfig{
		URL:       srv.URL,
		EventMask: []EventType{Warning},
	}, srv.Client())

	err := n.Send(t.Context(), Event{Type: Warning, Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error for 300 status code, got nil")
	}
}

// TestAppriseNotifier_ClientTimeoutPreventsHang proves that a hung Apprise
// endpoint returns an error rather than blocking Send indefinitely. It
// injects a client with a short timeout (rather than waiting out the real
// defaultAppriseTimeout) to keep the test fast.
func TestAppriseNotifier_ClientTimeoutPreventsHang(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block // never respond until the test unblocks it
	}))
	// httptest.Server.Close blocks until outstanding requests complete, so
	// the handler must be unblocked (close(block)) before Close is called —
	// hence a single Cleanup rather than a separate defer, which would run
	// first and deadlock waiting on the still-blocked handler.
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	client := &http.Client{Timeout: 50 * time.Millisecond}
	n := NewAppriseNotifier(AppriseConfig{
		URL:       srv.URL,
		EventMask: []EventType{Warning},
	}, client)

	start := time.Now()
	err := n.Send(t.Context(), Event{Type: Warning, Timestamp: time.Now()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Send to fail once the client timeout elapses, got nil error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Send took too long (%v); expected it to fail promptly at the client timeout", elapsed)
	}
}

// TestAppriseNotifier_DefaultTimeoutAppliedWhenClientTimeoutZero proves that
// a caller-supplied client with Timeout == 0 (e.g. constructed with
// &http.Client{} rather than http.DefaultClient) is also replaced with the
// bounded default, not just a nil client.
func TestAppriseNotifier_DefaultTimeoutAppliedWhenClientTimeoutZero(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := NewAppriseNotifier(AppriseConfig{
		URL:       srv.URL,
		EventMask: []EventType{Warning},
	}, &http.Client{}) // Timeout: 0, same hazard as http.DefaultClient

	err := n.Send(t.Context(), Event{Type: Warning, Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("expected success once the zero-timeout client is replaced, got: %v", err)
	}
}
