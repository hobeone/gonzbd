package app

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/notifier"
)

func TestBuildNotifier_ScriptTimeoutCases(t *testing.T) {
	cases := []struct {
		name            string
		inputTimeout    int
		expectedTimeout time.Duration
	}{
		{
			name:            "ZeroTimeoutDefaultsTo30s",
			inputTimeout:    0,
			expectedTimeout: 30 * time.Second,
		},
		{
			name:            "NegativeTimeoutDefaultsTo30s",
			inputTimeout:    -5,
			expectedTimeout: 30 * time.Second,
		},
		{
			name:            "PositiveTimeoutPreserved",
			inputTimeout:    10,
			expectedTimeout: 10 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NotificationConfig{
				Script: config.ScriptNotificationConfig{
					Enable:  true,
					Path:    "/tmp/notify.sh",
					Timeout: tc.inputTimeout,
				},
			}
			d := BuildNotifier(cfg)
			if d == nil {
				t.Fatal("BuildNotifier returned nil")
			}
			notifiers := d.Notifiers()
			if len(notifiers) != 1 {
				t.Fatalf("Expected exactly 1 notifier, got %d", len(notifiers))
			}
			sn := notifiers[0]
			if sn.Name() != "script" {
				t.Fatalf("Expected script notifier, got %q", sn.Name())
			}
			scriptNotifier, ok := sn.(*notifier.ScriptNotifier)
			if !ok {
				t.Fatalf("Expected *notifier.ScriptNotifier, got %T", sn)
			}
			scriptCfg := scriptNotifier.Config()
			if scriptCfg.Timeout != tc.expectedTimeout {
				t.Errorf("Expected timeout %v, got %v", tc.expectedTimeout, scriptCfg.Timeout)
			}
		})
	}
}
