package app

import (
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/notifier"
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

// ---------- sorterRulesFromConfig ----------

func TestSorterRulesFromConfig_Empty(t *testing.T) {
	t.Parallel()
	rules := sorterRulesFromConfig(nil)
	if len(rules) != 0 {
		t.Errorf("nil input: got %d rules, want 0", len(rules))
	}
}

func TestSorterRulesFromConfig_FiltersInactive(t *testing.T) {
	t.Parallel()
	cfgs := []config.SorterConfig{
		{Name: "active", IsActive: true, Order: 1, SortString: "%sn/%en"},
		{Name: "inactive", IsActive: false, Order: 0, SortString: "%sn"},
	}
	rules := sorterRulesFromConfig(cfgs)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1 (inactive filtered)", len(rules))
	}
	if rules[0].Name != "active" {
		t.Errorf("rule name = %q, want %q", rules[0].Name, "active")
	}
}

func TestSorterRulesFromConfig_OrderedByOrder(t *testing.T) {
	t.Parallel()
	cfgs := []config.SorterConfig{
		{Name: "third", IsActive: true, Order: 3},
		{Name: "first", IsActive: true, Order: 1},
		{Name: "second", IsActive: true, Order: 2},
	}
	rules := sorterRulesFromConfig(cfgs)
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if rules[i].Name != w {
			t.Errorf("rules[%d].Name = %q, want %q", i, rules[i].Name, w)
		}
	}
}

func TestSorterRulesFromConfig_FieldMapping(t *testing.T) {
	t.Parallel()
	cfgs := []config.SorterConfig{{
		Name:       "tv",
		IsActive:   true,
		Order:      1,
		SortType:   []int{1, 2},
		SortCats:   []string{"tv", "movie"},
		SortString: "%sn/Season %s",
		MinSize:    100,
	}}
	rules := sorterRulesFromConfig(cfgs)
	if len(rules) != 1 {
		t.Fatalf("got %d rules", len(rules))
	}
	r := rules[0]
	if !r.Enabled {
		t.Error("rule should be enabled")
	}
	if r.SortString != "%sn/Season %s" {
		t.Errorf("SortString = %q", r.SortString)
	}
	if r.Min != 100 {
		t.Errorf("Min = %d, want 100", r.Min)
	}
	if len(r.Categories) != 2 {
		t.Errorf("Categories = %v, want 2", r.Categories)
	}
	if len(r.Types) != 2 {
		t.Errorf("Types len = %d, want 2", len(r.Types))
	}
}

// ---------- SetEmitter ----------

func TestSetEmitter_NilUseDummy(t *testing.T) {
	t.Parallel()
	app := &Application{}
	app.SetEmitter(nil)
	if _, ok := app.emitter.(dummyEmitter); !ok {
		t.Error("nil emitter should set dummyEmitter")
	}
}

type mockEmitter struct{ called bool }

func (m *mockEmitter) Broadcast(_ Event) { m.called = true }

func TestSetEmitter_Custom(t *testing.T) {
	t.Parallel()
	app := &Application{}
	e := &mockEmitter{}
	app.SetEmitter(e)
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
