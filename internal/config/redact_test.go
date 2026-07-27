package config

import "testing"

func TestConfig_Redacted(t *testing.T) {
	t.Parallel()
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *Config) {
		c.General.APIKey = "realapikey1234567"
		c.General.NZBKey = "realnzbkey1234567"
		c.General.DownloadDir = "/data/incomplete"
		c.Servers = []ServerConfig{
			{Name: "s1", Host: "news.example.com", Password: "hunter2"},
			{Name: "s2", Host: "news2.example.com", Password: ""},
		}
		c.Notifications.Email.Password = "smtppass"
		c.Notifications.Email.Host = "smtp.example.com"
		c.Notifications.Apprise.URL = "discord://webhook_id/webhook_token"
		c.Notifications.Apprise.ServiceURL = "https://apprise.example.com/notify"
	})

	redacted := cfg.Redacted()

	if redacted.General.APIKey != RedactedPlaceholder {
		t.Errorf("General.APIKey = %q; want redacted", redacted.General.APIKey)
	}
	if redacted.General.NZBKey != RedactedPlaceholder {
		t.Errorf("General.NZBKey = %q; want redacted", redacted.General.NZBKey)
	}
	if redacted.General.DownloadDir != "/data/incomplete" {
		t.Errorf("General.DownloadDir = %q; want unchanged", redacted.General.DownloadDir)
	}
	if redacted.Servers[0].Password != RedactedPlaceholder {
		t.Errorf("Servers[0].Password = %q; want redacted", redacted.Servers[0].Password)
	}
	if redacted.Servers[0].Host != "news.example.com" {
		t.Errorf("Servers[0].Host = %q; want unchanged", redacted.Servers[0].Host)
	}
	if redacted.Servers[1].Password != "" {
		t.Errorf("Servers[1].Password = %q; want left empty, not redacted-with-placeholder", redacted.Servers[1].Password)
	}
	if redacted.Notifications.Email.Password != RedactedPlaceholder {
		t.Errorf("Notifications.Email.Password = %q; want redacted", redacted.Notifications.Email.Password)
	}
	if redacted.Notifications.Email.Host != "smtp.example.com" {
		t.Errorf("Notifications.Email.Host = %q; want unchanged", redacted.Notifications.Email.Host)
	}
	if redacted.Notifications.Apprise.URL != RedactedPlaceholder {
		t.Errorf("Notifications.Apprise.URL = %q; want redacted", redacted.Notifications.Apprise.URL)
	}
	if redacted.Notifications.Apprise.ServiceURL != RedactedPlaceholder {
		t.Errorf("Notifications.Apprise.ServiceURL = %q; want redacted", redacted.Notifications.Apprise.ServiceURL)
	}

	// The original config must be untouched.
	snap := cfg.Snapshot()
	if snap.General.APIKey != "realapikey1234567" {
		t.Errorf("original General.APIKey was mutated: %q", snap.General.APIKey)
	}
	if snap.Servers[0].Password != "hunter2" {
		t.Errorf("original Servers[0].Password was mutated: %q", snap.Servers[0].Password)
	}
}
