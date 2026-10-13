package app

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/notifier"
)

func TestBuildNotifier_ScriptTimeoutCases(t *testing.T) {
	cases := []struct {
		name            string
		inputTimeout    int
		expectedTimeout time.Duration
	}{
		{
			name:            "ZeroTimeoutTakesDefault",
			inputTimeout:    0,
			expectedTimeout: constants.DefaultScriptTimeout,
		},
		{
			name:            "NegativeTimeoutTakesDefault",
			inputTimeout:    -5,
			expectedTimeout: constants.DefaultScriptTimeout,
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

func TestBuildNotifier_SinksCarryConfig(t *testing.T) {
	t.Parallel()
	cfg := config.NotificationConfig{
		Email: config.EmailNotificationConfig{
			Enable:   true,
			Host:     "smtp.invalid",
			Port:     587,
			Username: "u",
			Password: "p",
			From:     "from@invalid",
			To:       []string{"to@invalid"},
			UseTLS:   true,
			Events:   []string{"DownloadFailed"},
		},
		Apprise: config.AppriseNotificationConfig{
			Enable:     true,
			URL:        "http://apprise.invalid/notify",
			ServiceURL: "json://svc.invalid",
			Events:     []string{"QueueDone"},
		},
		Script: config.ScriptNotificationConfig{
			Enable: true,
			Path:   "/bin/true",
			Events: []string{"Warning"},
		},
	}
	ns := BuildNotifier(cfg).Notifiers()
	if len(ns) != 3 {
		t.Fatalf("got %d notifiers, want 3", len(ns))
	}

	en, ok := ns[0].(*notifier.EmailNotifier)
	if !ok {
		t.Fatalf("notifier 0 is %T, want *notifier.EmailNotifier", ns[0])
	}
	ec := en.Config()
	if ec.Host != "smtp.invalid" || ec.Port != 587 || ec.Username != "u" || ec.Password != "p" ||
		ec.From != "from@invalid" || len(ec.To) != 1 || ec.To[0] != "to@invalid" || !ec.UseTLS || ec.UseSSL {
		t.Errorf("email config not carried through: %+v", ec)
	}
	if !en.Accepts(notifier.DownloadFailed) || en.Accepts(notifier.QueueDone) {
		t.Error("email event mask not taken from cfg.Email.Events")
	}

	an, ok := ns[1].(*notifier.AppriseNotifier)
	if !ok {
		t.Fatalf("notifier 1 is %T, want *notifier.AppriseNotifier", ns[1])
	}
	ac := an.Config()
	if ac.URL != "http://apprise.invalid/notify" || ac.ServiceURL != "json://svc.invalid" {
		t.Errorf("apprise config not carried through: %+v", ac)
	}
	if !an.Accepts(notifier.QueueDone) || an.Accepts(notifier.DownloadFailed) {
		t.Error("apprise event mask not taken from cfg.Apprise.Events")
	}

	sn, ok := ns[2].(*notifier.ScriptNotifier)
	if !ok {
		t.Fatalf("notifier 2 is %T, want *notifier.ScriptNotifier", ns[2])
	}
	if sn.Config().Path != "/bin/true" {
		t.Errorf("script path not carried through: %q", sn.Config().Path)
	}
	if !sn.Accepts(notifier.Warning) || sn.Accepts(notifier.DownloadFailed) || sn.Accepts(notifier.QueueDone) {
		t.Error("script event mask not taken from cfg.Script.Events")
	}
}
