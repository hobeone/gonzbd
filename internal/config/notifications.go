package config

// NotificationConfig controls event notification sinks (email, Apprise,
// script). Each sub-section is independent; leave a section empty to
// disable that sink. See spec §9.8.
type NotificationConfig struct {
	// Email holds SMTP settings for email notifications.
	Email EmailNotificationConfig `yaml:"email,omitempty" json:"email"`

	// Apprise holds settings for an Apprise API endpoint.
	Apprise AppriseNotificationConfig `yaml:"apprise,omitempty" json:"apprise"`

	// Script holds settings for a notification script.
	Script ScriptNotificationConfig `yaml:"script,omitempty" json:"script"`
}

// EmailNotificationConfig maps to notifier.EmailConfig.
type EmailNotificationConfig struct {
	Enable   bool     `yaml:"enable" json:"enable"`
	Host     string   `yaml:"host" json:"host"`
	Port     int      `yaml:"port" json:"port"`
	Username string   `yaml:"username" json:"username"`
	Password string   `yaml:"password" json:"password"`
	From     string   `yaml:"from" json:"from"`
	To       []string `yaml:"to" json:"to"`
	UseTLS   bool     `yaml:"use_tls" json:"use_tls"`
	UseSSL   bool     `yaml:"use_ssl" json:"use_ssl"`
	// Events is a list of event type names to subscribe to. Empty means all.
	// Valid names: DownloadStarted, DownloadComplete, DownloadFailed,
	// PostProcessingComplete, PostProcessingFailed, DiskFull, QueueDone,
	// Warning, Error.
	Events []string `yaml:"events,omitempty" json:"events,omitempty"`
}

// AppriseNotificationConfig maps to notifier.AppriseConfig.
type AppriseNotificationConfig struct {
	Enable     bool     `yaml:"enable" json:"enable"`
	URL        string   `yaml:"url" json:"url"`
	ServiceURL string   `yaml:"service_url" json:"service_url"`
	Events     []string `yaml:"events,omitempty" json:"events,omitempty"`
}

// ScriptNotificationConfig maps to notifier.ScriptConfig.
type ScriptNotificationConfig struct {
	Enable  bool     `yaml:"enable" json:"enable"`
	Path    string   `yaml:"path" json:"path"`
	Timeout int      `yaml:"timeout" json:"timeout"` // seconds; 0 = 30s default
	Events  []string `yaml:"events,omitempty" json:"events,omitempty"`
}
