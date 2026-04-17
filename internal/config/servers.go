package config

// ServerConfig describes a single NNTP server entry. See spec §9.6 and
// §3.6 for SSL/penalty semantics.
type ServerConfig struct {
	// Name is the unique server identifier within the config. Used as
	// the lookup key by other subsystems (try-lists, penalty tracking).
	// Required.
	Name string `yaml:"name" json:"name"`
	// DisplayName is the user-facing label; falls back to Name when empty.
	DisplayName string `yaml:"displayname" json:"displayname"`

	// Host is the hostname or IP literal.
	Host string `yaml:"host" json:"host"`
	// Port is the TCP port; defaults are 119 (plain) and 563 (SSL).
	Port int `yaml:"port" json:"port"`

	// Username for AUTHINFO. Empty for anonymous servers.
	Username string `yaml:"username" json:"username"`
	// Password for AUTHINFO.
	Password string `yaml:"password" json:"password"`

	// Connections is the number of simultaneous NNTP connections to open.
	// Must be >= 1.
	Connections int `yaml:"connections" json:"connections"`

	// SSL enables TLS on connect.
	SSL bool `yaml:"ssl" json:"ssl"`
	// SSLVerify selects the certificate-verification level. See spec §3.3.
	SSLVerify SSLVerify `yaml:"ssl_verify" json:"ssl_verify"`
	// SSLCiphers is an OpenSSL cipher string; empty selects defaults.
	SSLCiphers string `yaml:"ssl_ciphers" json:"ssl_ciphers"`

	// Priority dispatch order; 0 is highest. Lower-priority servers
	// receive an article only after all higher-priority servers have
	// either tried it or are otherwise ineligible.
	Priority int `yaml:"priority" json:"priority"`

	// Required marks the server as essential — the dispatcher will not
	// auto-deactivate it under penalty pressure.
	Required bool `yaml:"required" json:"required"`
	// Optional marks the server as a backup — eligible for temporary
	// deactivation when the bad-connection ratio crosses
	// constants.OptionalDeactivationThreshold.
	Optional bool `yaml:"optional" json:"optional"`

	// Retention is the article-retention window in days. 0 = unlimited.
	// Articles older than the retention horizon are not requested from
	// this server.
	Retention int `yaml:"retention" json:"retention"`

	// Timeout is the per-request socket timeout in seconds.
	Timeout int `yaml:"timeout" json:"timeout"`

	// PipeliningRequests is the max in-flight commands per connection.
	// Default 2. Servers that misbehave under pipelining can be set to 1.
	PipeliningRequests int `yaml:"pipelining_requests" json:"pipelining_requests"`

	// Enable allows the server to be used. Disabled servers are kept in
	// config but skipped at dispatch.
	Enable bool `yaml:"enable" json:"enable"`
}
