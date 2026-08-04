package config

import (
	"strings"
	"testing"
)

// TestGeneralConfig_HistoryRetentionRoundTrip pins that the two retention
// thresholds parse from YAML and survive into the typed config.
//
// They are day counts with 0 meaning "keep forever" (spec §11.4), split by
// outcome so a failed job can be kept longer than a successful one — a
// failure is the case an operator is most likely to want to look at later.
func TestGeneralConfig_HistoryRetentionRoundTrip(t *testing.T) {
	cfg, _, err := decode(strings.NewReader(`
general:
  history_retention_days: 30
  history_failed_retention_days: 7
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.General.HistoryRetentionDays != 30 {
		t.Errorf("HistoryRetentionDays = %d, want 30", cfg.General.HistoryRetentionDays)
	}
	if cfg.General.HistoryFailedRetentionDays != 7 {
		t.Errorf("HistoryFailedRetentionDays = %d, want 7", cfg.General.HistoryFailedRetentionDays)
	}
}

// TestGeneralConfig_HistoryRetentionDefaultsToKeepForever pins that an
// operator who has not configured retention keeps everything.
//
// Zero is load-bearing rather than merely a zero value: the pruner treats it
// as "no threshold", so a defaulting mistake here silently deletes history
// instead of silently keeping it.
func TestGeneralConfig_HistoryRetentionDefaultsToKeepForever(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.General.HistoryRetentionDays != 0 {
		t.Errorf("default HistoryRetentionDays = %d, want 0 (keep forever)",
			cfg.General.HistoryRetentionDays)
	}
	if cfg.General.HistoryFailedRetentionDays != 0 {
		t.Errorf("default HistoryFailedRetentionDays = %d, want 0 (keep forever)",
			cfg.General.HistoryFailedRetentionDays)
	}
}

// TestGeneralConfig_HistoryRetentionRejectsNegative pins that a negative
// threshold is a config error rather than being coerced.
//
// A negative day count would produce a cutoff in the future, making every
// entry expired — silently wiping history on the next sweep.
func TestGeneralConfig_HistoryRetentionRejectsNegative(t *testing.T) {
	for _, field := range []string{"history_retention_days", "history_failed_retention_days"} {
		t.Run(field, func(t *testing.T) {
			cfg, err := Default()
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			switch field {
			case "history_retention_days":
				cfg.General.HistoryRetentionDays = -1
			default:
				cfg.General.HistoryFailedRetentionDays = -1
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("a negative %s was accepted", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name %q", err, field)
			}
		})
	}
}
