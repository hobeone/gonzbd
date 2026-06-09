package app

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/notifier"
)

func getNotifiers(d *notifier.Dispatcher) []notifier.Notifier {
	val := reflect.ValueOf(d).Elem()
	field := val.FieldByName("notifiers")
	ptr := unsafe.Pointer(field.UnsafeAddr())
	return *(*[]notifier.Notifier)(ptr)
}

func getScriptConfig(sn notifier.Notifier) notifier.ScriptConfig {
	val := reflect.ValueOf(sn).Elem()
	field := val.FieldByName("cfg")
	ptr := unsafe.Pointer(field.UnsafeAddr())
	return *(*notifier.ScriptConfig)(ptr)
}

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
			notifiers := getNotifiers(d)
			if len(notifiers) != 1 {
				t.Fatalf("Expected exactly 1 notifier, got %d", len(notifiers))
			}
			sn := notifiers[0]
			if sn.Name() != "script" {
				t.Fatalf("Expected script notifier, got %q", sn.Name())
			}
			scriptCfg := getScriptConfig(sn)
			if scriptCfg.Timeout != tc.expectedTimeout {
				t.Errorf("Expected timeout %v, got %v", tc.expectedTimeout, scriptCfg.Timeout)
			}
		})
	}
}
