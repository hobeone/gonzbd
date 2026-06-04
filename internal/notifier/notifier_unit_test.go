package notifier

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------- EventType.String ----------

func TestEventType_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
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
		{EventType(99), "EventType(99)"},
	}
	for _, tt := range tests {
		got := tt.et.String()
		if got != tt.want {
			t.Errorf("EventType(%d).String() = %q, want %q", tt.et, got, tt.want)
		}
	}
}

// ---------- acceptsAny ----------

func TestAcceptsAny_Found(t *testing.T) {
	t.Parallel()
	mask := []EventType{DownloadComplete, Error}
	if !acceptsAny(mask, DownloadComplete) {
		t.Error("expected true for DownloadComplete")
	}
}

func TestAcceptsAny_NotFound(t *testing.T) {
	t.Parallel()
	mask := []EventType{DownloadComplete, Error}
	if acceptsAny(mask, DiskFull) {
		t.Error("expected false for DiskFull")
	}
}

func TestAcceptsAny_EmptyMask(t *testing.T) {
	t.Parallel()
	if acceptsAny(nil, DownloadComplete) {
		t.Error("expected false for empty mask")
	}
}

// ---------- Dispatcher ----------

type mockNotifier struct {
	name    string
	mask    []EventType
	sendErr error
	sent    []Event
}

func (m *mockNotifier) Name() string              { return m.name }
func (m *mockNotifier) Accepts(et EventType) bool { return acceptsAny(m.mask, et) }
func (m *mockNotifier) Send(_ context.Context, e Event) error {
	m.sent = append(m.sent, e)
	return m.sendErr
}

func TestDispatcher_DispatchFilters(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(nil)
	n1 := &mockNotifier{name: "n1", mask: []EventType{DownloadComplete}}
	n2 := &mockNotifier{name: "n2", mask: []EventType{Error}}
	d.Register(n1)
	d.Register(n2)

	d.Dispatch(t.Context(), Event{
		Type:  DownloadComplete,
		Title: "Job Done",
	})

	if len(n1.sent) != 1 {
		t.Errorf("n1 received %d events, want 1", len(n1.sent))
	}
	if len(n2.sent) != 0 {
		t.Errorf("n2 received %d events, want 0 (filtered)", len(n2.sent))
	}
}

func TestDispatcher_DispatchNoNotifiers(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(nil)
	// Should not panic.
	d.Dispatch(t.Context(), Event{Type: Warning, Title: "test"})
}

func TestDispatcher_DispatchErrorSwallowed(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(nil)
	failing := &mockNotifier{
		name:    "fail",
		mask:    []EventType{Error},
		sendErr: errors.New("send failed"),
	}
	d.Register(failing)

	// Should not panic; errors are logged but swallowed.
	d.Dispatch(t.Context(), Event{Type: Error, Title: "bad"})
}

// ---------- ScriptNotifier ----------

func TestScriptNotifier_Name(t *testing.T) {
	t.Parallel()
	sn := NewScriptNotifier(ScriptConfig{Path: "/bin/echo", Timeout: 5 * time.Second})
	if sn.Name() != "script" {
		t.Errorf("Name() = %q, want %q", sn.Name(), "script")
	}
}

func TestScriptNotifier_Accepts(t *testing.T) {
	t.Parallel()
	sn := NewScriptNotifier(ScriptConfig{
		Path:      "/bin/echo",
		EventMask: []EventType{DownloadComplete, QueueDone},
	})
	if !sn.Accepts(DownloadComplete) {
		t.Error("should accept DownloadComplete")
	}
	if sn.Accepts(Error) {
		t.Error("should not accept Error")
	}
}

func TestScriptNotifier_EmptyMask(t *testing.T) {
	t.Parallel()
	// Empty EventMask at the notifier level means accept nothing.
	// The "accept all" logic is in parseEventMask which populates
	// the mask with all event types before passing to the notifier.
	sn := NewScriptNotifier(ScriptConfig{Path: "/bin/echo"})
	if sn.Accepts(DownloadComplete) {
		t.Error("empty mask should accept nothing at notifier level")
	}
}

// ---------- AppriseNotifier ----------

func TestAppriseNotifier_Name(t *testing.T) {
	t.Parallel()
	an := NewAppriseNotifier(AppriseConfig{URL: "http://apprise"}, nil)
	if an.Name() != "apprise" {
		t.Errorf("Name() = %q, want %q", an.Name(), "apprise")
	}
}

func TestAppriseNotifier_Accepts(t *testing.T) {
	t.Parallel()
	an := NewAppriseNotifier(AppriseConfig{
		URL:       "http://apprise",
		EventMask: []EventType{Warning},
	}, nil)
	if !an.Accepts(Warning) {
		t.Error("should accept Warning")
	}
	if an.Accepts(DownloadStarted) {
		t.Error("should not accept DownloadStarted")
	}
}

// ---------- EmailNotifier ----------

func TestEmailNotifier_Name(t *testing.T) {
	t.Parallel()
	en := NewEmailNotifier(EmailConfig{Host: "smtp.example.com", Port: 587})
	if en.Name() != "email" {
		t.Errorf("Name() = %q, want %q", en.Name(), "email")
	}
}

func TestEmailNotifier_Accepts(t *testing.T) {
	t.Parallel()
	en := NewEmailNotifier(EmailConfig{
		Host:      "smtp.example.com",
		Port:      587,
		EventMask: []EventType{PostProcessingComplete},
	})
	if !en.Accepts(PostProcessingComplete) {
		t.Error("should accept PostProcessingComplete")
	}
	if en.Accepts(DiskFull) {
		t.Error("should not accept DiskFull")
	}
}

func TestAppriseTypeDirect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input EventType
		want  string
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
		{EventType(99), "info"},
	}

	for _, tc := range cases {
		t.Run(tc.input.String(), func(t *testing.T) {
			got := appriseType(tc.input)
			if got != tc.want {
				t.Errorf("appriseType(%s) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
