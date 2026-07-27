package config

// RedactedPlaceholder replaces credential values in Redacted() output.
const RedactedPlaceholder = "********"

// Redacted returns a copy of the config with all credential and secret
// fields replaced by RedactedPlaceholder, safe to serialize and display
// (e.g. the status page's Config panel) without leaking secrets.
//
// This is a field allowlist of known-sensitive values, not a generic
// deep-copy — new sensitive fields added to the config structs must be
// added here explicitly.
func (c *Config) Redacted() *Config {
	out := c.Snapshot()

	if out.General.APIKey != "" {
		out.General.APIKey = RedactedPlaceholder
	}
	if out.General.NZBKey != "" {
		out.General.NZBKey = RedactedPlaceholder
	}
	for i := range out.Servers {
		if out.Servers[i].Password != "" {
			out.Servers[i].Password = RedactedPlaceholder
		}
	}
	if out.Notifications.Email.Password != "" {
		out.Notifications.Email.Password = RedactedPlaceholder
	}
	// Apprise URLs commonly embed the credential itself (e.g. a Discord
	// or Slack webhook token is part of the URL, not a separate field).
	if out.Notifications.Apprise.URL != "" {
		out.Notifications.Apprise.URL = RedactedPlaceholder
	}
	if out.Notifications.Apprise.ServiceURL != "" {
		out.Notifications.Apprise.ServiceURL = RedactedPlaceholder
	}

	return out
}
