package app

import (
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/notifier"
)

// eventNameToType maps config event name strings to notifier.EventType constants.
var eventNameToType = map[string]notifier.EventType{
	"DownloadStarted":        notifier.DownloadStarted,
	"DownloadComplete":       notifier.DownloadComplete,
	"DownloadFailed":         notifier.DownloadFailed,
	"PostProcessingComplete": notifier.PostProcessingComplete,
	"PostProcessingFailed":   notifier.PostProcessingFailed,
	"DiskFull":               notifier.DiskFull,
	"QueueDone":              notifier.QueueDone,
	"Warning":                notifier.Warning,
	"Error":                  notifier.Error,
}

// allEventTypes is the full event type list used when no filter is specified.
var allEventTypes = []notifier.EventType{
	notifier.DownloadStarted,
	notifier.DownloadComplete,
	notifier.DownloadFailed,
	notifier.PostProcessingComplete,
	notifier.PostProcessingFailed,
	notifier.DiskFull,
	notifier.QueueDone,
	notifier.Warning,
	notifier.Error,
}

// parseEventMask converts string event names to EventType values. If names
// is empty, all events are returned (subscribe to everything).
func parseEventMask(names []string) []notifier.EventType {
	if len(names) == 0 {
		return allEventTypes
	}
	mask := make([]notifier.EventType, 0, len(names))
	for _, n := range names {
		if t, ok := eventNameToType[n]; ok {
			mask = append(mask, t)
		}
	}
	return mask
}

// BuildNotifier constructs a Dispatcher from the config's notification
// section, registering any enabled sinks (email, apprise, script).
func BuildNotifier(cfg config.NotificationConfig) *notifier.Dispatcher {
	d := notifier.NewDispatcher(nil)

	if cfg.Email.Enable {
		d.Register(notifier.NewEmailNotifier(notifier.EmailConfig{
			Host:      cfg.Email.Host,
			Port:      cfg.Email.Port,
			Username:  cfg.Email.Username,
			Password:  cfg.Email.Password,
			From:      cfg.Email.From,
			To:        cfg.Email.To,
			UseTLS:    cfg.Email.UseTLS,
			UseSSL:    cfg.Email.UseSSL,
			EventMask: parseEventMask(cfg.Email.Events),
		}))
	}

	if cfg.Apprise.Enable {
		d.Register(notifier.NewAppriseNotifier(notifier.AppriseConfig{
			URL:        cfg.Apprise.URL,
			ServiceURL: cfg.Apprise.ServiceURL,
			EventMask:  parseEventMask(cfg.Apprise.Events),
		}, nil))
	}

	if cfg.Script.Enable {
		timeout := time.Duration(cfg.Script.Timeout) * time.Second
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		d.Register(notifier.NewScriptNotifier(notifier.ScriptConfig{
			Path:      cfg.Script.Path,
			Timeout:   timeout,
			EventMask: parseEventMask(cfg.Script.Events),
		}))
	}

	return d
}
