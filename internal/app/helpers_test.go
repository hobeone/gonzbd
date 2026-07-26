package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/notifier"
)

// ---------- parseEventMask ----------

func TestParseEventMask_Empty(t *testing.T) {
	t.Parallel()
	mask := parseEventMask(nil)
	if len(mask) != len(allEventTypes) {
		t.Errorf("empty input: got %d events, want %d (all)", len(mask), len(allEventTypes))
	}
}

func TestParseEventMask_ValidNames(t *testing.T) {
	t.Parallel()
	names := []string{"DownloadComplete", "Error"}
	mask := parseEventMask(names)
	if len(mask) != 2 {
		t.Fatalf("got %d events, want 2", len(mask))
	}
	if mask[0] != notifier.DownloadComplete {
		t.Errorf("mask[0] = %v, want DownloadComplete", mask[0])
	}
	if mask[1] != notifier.Error {
		t.Errorf("mask[1] = %v, want Error", mask[1])
	}
}

func TestParseEventMask_UnknownNames(t *testing.T) {
	t.Parallel()
	mask := parseEventMask([]string{"Nonexistent", "AlsoFake"})
	if len(mask) != 0 {
		t.Errorf("unknown names: got %d events, want 0", len(mask))
	}
}

func TestParseEventMask_MixedValid(t *testing.T) {
	t.Parallel()
	mask := parseEventMask([]string{"Warning", "Fake", "QueueDone"})
	if len(mask) != 2 {
		t.Errorf("mixed input: got %d events, want 2", len(mask))
	}
}

// ---------- BuildNotifier ----------

func TestBuildNotifier_NoSinks(t *testing.T) {
	t.Parallel()
	d := BuildNotifier(config.NotificationConfig{})
	if d == nil {
		t.Fatal("BuildNotifier returned nil")
	}
}

func TestBuildNotifier_AllSinksDisabled(t *testing.T) {
	t.Parallel()
	cfg := config.NotificationConfig{
		Email:   config.EmailNotificationConfig{Enable: false},
		Apprise: config.AppriseNotificationConfig{Enable: false},
		Script:  config.ScriptNotificationConfig{Enable: false},
	}
	d := BuildNotifier(cfg)
	if d == nil {
		t.Fatal("BuildNotifier returned nil")
	}
}

func TestBuildNotifier_ScriptEnabled(t *testing.T) {
	t.Parallel()
	cfg := config.NotificationConfig{
		Script: config.ScriptNotificationConfig{
			Enable:  true,
			Path:    "/tmp/notify.sh",
			Timeout: 10,
			Events:  []string{"DownloadComplete"},
		},
	}
	d := BuildNotifier(cfg)
	if d == nil {
		t.Fatal("BuildNotifier returned nil")
	}
}

func TestBuildNotifier_ScriptDefaultTimeout(t *testing.T) {
	t.Parallel()
	cfg := config.NotificationConfig{
		Script: config.ScriptNotificationConfig{
			Enable:  true,
			Path:    "/tmp/notify.sh",
			Timeout: 0, // should default to 30s
		},
	}
	d := BuildNotifier(cfg)
	if d == nil {
		t.Fatal("BuildNotifier returned nil")
	}
}

// ---------- WithEventEmitter ----------

func TestWithEventEmitter_NilUseDummy(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil, WithEventEmitter(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := app.emitter.(dummyEmitter); !ok {
		t.Error("nil emitter should set dummyEmitter")
	}
}

type mockEmitter struct{ called bool }

func (m *mockEmitter) Broadcast(_ Event) { m.called = true }

func TestWithEventEmitter_Custom(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	e := &mockEmitter{}
	app, err := New(cfg, nil, WithEventEmitter(e))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	app.emitter.Broadcast(Event{})
	if !e.called {
		t.Error("custom emitter Broadcast not called")
	}
}

// ---------- SetNotifier ----------

func TestSetNotifier(t *testing.T) {
	t.Parallel()
	app := &Application{}
	d := notifier.NewDispatcher(nil)
	app.SetNotifier(d)
	if app.notifyDispatcher != d {
		t.Error("dispatcher not set")
	}
}

// ---------- dummyEmitter ----------

func TestDummyEmitter_Broadcast(t *testing.T) {
	t.Parallel()
	// Should not panic.
	d := dummyEmitter{}
	d.Broadcast(Event{Type: "test"})
}
