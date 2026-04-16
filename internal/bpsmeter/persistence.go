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

// Capture builds a State from the current Meter and Quota snapshots.
// Use this before calling SaveState.
func Capture(m *Meter, q *Quota) State {
	snap := m.Snapshot()
	usage, _ := q.UsageAndBudget()
	ps := q.PeriodStart()

	servers := make(map[string]int64, len(snap.Servers))
	for name, ss := range snap.Servers {
		servers[name] = ss.Total
	}
	return State{
		PeriodStart:   ps,
		PeriodUsage:   usage,
		LifetimeTotal: snap.Total,
		ServerTotals:  servers,
	}
}

// Restore applies a previously loaded State to fresh Meter and Quota instances.
// It records the persisted byte counts without affecting the rolling-window BPS
// (which starts fresh because historical samples are not stored).
func Restore(m *Meter, q *Quota, s State) {
	// Replay lifetime totals into Meter via Record so the internal maps are populated.
	// We record all bytes at once; BPS will be 0 until fresh traffic arrives.
	if s.LifetimeTotal > 0 {
		m.Record("", s.LifetimeTotal)
	}
	// Subtract the aggregate we just added because Record also adds to aggregate;
	// instead build it per-server and let aggregate accumulate naturally.
	// Reset aggregate to avoid double-counting: re-create meter state directly.
	// Simpler: use internal lock to set lifetime directly.
	m.mu.Lock()
	// Reset what Record just wrote to aggregate.
	m.servers[""].lifetime = s.LifetimeTotal
	// Remove the bucket bytes Record added (avoid phantom BPS spike on restore).
	agg := m.servers[""]
	for i := range agg.buckets {
		agg.buckets[i] = bucket{}
	}
	// Restore per-server totals.
	for srv, total := range s.ServerTotals {
		ss := m.getOrCreate(srv)
		ss.lifetime = total
		// Clear any buckets written by concurrent code during restore.
		for i := range ss.buckets {
			ss.buckets[i] = bucket{}
		}
	}
	m.mu.Unlock()

	// Restore quota period usage.
	q.mu.Lock()
	if !s.PeriodStart.IsZero() {
		q.periodStart = s.PeriodStart
	}
	q.usage = s.PeriodUsage
	// Re-evaluate exceeded flag without firing handler.
	if q.cfg.Budget > 0 && q.usage >= q.cfg.Budget {
		q.exceeded = true
	}
	q.mu.Unlock()
}
