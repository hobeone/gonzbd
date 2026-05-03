package bpsmeter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the JSON-persisted shape for Quota + Meter lifetime totals.
type State struct {
	PeriodStart   time.Time        `json:"period_start"`
	PeriodUsage   int64            `json:"period_usage"`
	LifetimeTotal int64            `json:"lifetime_total"`
	ServerTotals  map[string]int64 `json:"server_totals"`
}

// LoadState reads and decodes a State from the file at path.
// Returns a zero State and an error if the file does not exist or is malformed.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is caller-supplied config
	if err != nil {
		return State{}, fmt.Errorf("LoadState: %w", err)
	}
	var s State
	if err = json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("LoadState: %w", err)
	}
	return s, nil
}

// SaveState atomically writes s as JSON to path by writing a temp file in
// the same directory and renaming it, ensuring readers never see a partial write.
func SaveState(path string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("SaveState: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bpsmeter-*.tmp")
	if err != nil {
		return fmt.Errorf("SaveState create temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err = tmp.Write(data); err != nil {
		//nolint:errcheck // best-effort cleanup; original write error takes precedence
		_ = tmp.Close()
		//nolint:errcheck // best-effort cleanup; original write error takes precedence
		_ = os.Remove(tmpName)
		return fmt.Errorf("SaveState write: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		//nolint:errcheck // best-effort cleanup; sync error takes precedence
		_ = tmp.Close()
		//nolint:errcheck // best-effort cleanup
		_ = os.Remove(tmpName)
		return fmt.Errorf("SaveState sync: %w", err)
	}
	if err = tmp.Close(); err != nil {
		//nolint:errcheck // best-effort cleanup; close error takes precedence
		_ = os.Remove(tmpName)
		return fmt.Errorf("SaveState close: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		//nolint:errcheck // best-effort cleanup; rename error takes precedence
		_ = os.Remove(tmpName)
		return fmt.Errorf("SaveState rename: %w", err)
	}
	return nil
}

// Capture builds a State from the current Meter snapshot.
// Use this before calling SaveState.
func Capture(m *Meter) State {
	snap := m.Snapshot()

	servers := make(map[string]int64, len(snap.Servers))
	for name, ss := range snap.Servers {
		servers[name] = ss.Total
	}
	return State{
		LifetimeTotal: snap.Total,
		ServerTotals:  servers,
	}
}

// Restore applies a previously loaded State to a fresh Meter.
// It sets the persisted lifetime totals directly without affecting the
// rolling-window BPS (which starts fresh because historical samples are not
// stored).
func Restore(m *Meter, s State) {
	// Set lifetime totals directly — avoids routing through Record, which
	// would pollute rolling-window buckets and require an immediate undo.
	m.mu.Lock()
	agg := m.getOrCreate("")
	agg.lifetime = s.LifetimeTotal
	for srv, total := range s.ServerTotals {
		ss := m.getOrCreate(srv)
		ss.lifetime = total
	}
	m.mu.Unlock()
}
