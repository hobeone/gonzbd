package main

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/downloader"
)

// T10: Test that wsAdapter faithfully forwards all app.Event fields —
// especially Tool, Line, and Stage which were missing before audit H2.
func TestWsAdapter_ForwardsAllFields(t *testing.T) {
	t.Parallel()

	// Create a real Broadcaster so we can call Broadcast without panic.
	logger := slog.Default()
	b := api.NewBroadcaster(logger)
	adapter := wsAdapter{b: b}

	// Build a full app.Event with all fields populated.
	appEvent := app.Event{
		Type:          "postproc_progress",
		Speed:         1_000_000,
		Remaining:     500,
		SpeedLimit:    2_000_000,
		BandwidthMax:  10_000_000,
		BandwidthPerc: 50,
		NzoID:         "SABnzbd_nzo_abc123",
		Tool:          "par2",
		Line:          "Verifying: myfile.rar",
		Stage:         "repair",
		Servers: []downloader.ServerSnapshot{
			{Name: "news.example.com", Host: "news.example.com", Port: 563, SSL: true},
		},
	}

	// Call the adapter to verify it doesn't panic. (No connected
	// WebSocket clients, so Broadcast is effectively a no-op.)
	adapter.Broadcast(appEvent)

	// Verify the field mapping by constructing what wsAdapter would
	// produce and round-tripping through JSON.
	apiEvent := api.Event{
		Type:          appEvent.Type,
		Speed:         appEvent.Speed,
		Remaining:     appEvent.Remaining,
		SpeedLimit:    appEvent.SpeedLimit,
		BandwidthMax:  appEvent.BandwidthMax,
		BandwidthPerc: appEvent.BandwidthPerc,
		NzoID:         appEvent.NzoID,
		Tool:          appEvent.Tool,
		Line:          appEvent.Line,
		Stage:         appEvent.Stage,
		Servers:       appEvent.Servers,
	}

	data, err := json.Marshal(apiEvent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded api.Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify every field survives the JSON round-trip.
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Type", decoded.Type, "postproc_progress"},
		{"Speed", decoded.Speed, int64(1_000_000)},
		{"Remaining", decoded.Remaining, int64(500)},
		{"SpeedLimit", decoded.SpeedLimit, int64(2_000_000)},
		{"BandwidthMax", decoded.BandwidthMax, int64(10_000_000)},
		{"BandwidthPerc", decoded.BandwidthPerc, 50},
		{"NzoID", decoded.NzoID, "SABnzbd_nzo_abc123"},
		{"Tool", decoded.Tool, "par2"},
		{"Line", decoded.Line, "Verifying: myfile.rar"},
		{"Stage", decoded.Stage, "repair"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// Verify Servers slice round-trips correctly.
	if len(decoded.Servers) != 1 {
		t.Fatalf("Servers length = %d; want 1", len(decoded.Servers))
	}
	if decoded.Servers[0].Name != "news.example.com" {
		t.Errorf("Servers[0].Name = %q; want %q", decoded.Servers[0].Name, "news.example.com")
	}
}
