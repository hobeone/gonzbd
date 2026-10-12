package config

import (
	"encoding/json"

	"gopkg.in/yaml.v3"
)

// ServerConfig describes a single NNTP server entry. See spec §9.6 and
// §3.6 for SSL/penalty semantics.
type ServerConfig struct {
	// Name is the unique server identifier within the config. Used as
	// the lookup key by other subsystems (try-lists, penalty tracking).
	// Required.
	Name string `yaml:"name" json:"name"`

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
	// An entry that leaves the key out gets DefaultSSLVerify.
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
	// Required and must be positive — validate rejects a zero rather
	// than defaulting it, so there is no code default; the sample
	// config and README use 2. Servers that misbehave under pipelining
	// can be set to 1.
	PipeliningRequests int `yaml:"pipelining_requests" json:"pipelining_requests"`

	// Enable allows the server to be used. Disabled servers are kept in
	// config but skipped at dispatch.
	Enable bool `yaml:"enable" json:"enable"`
}

// DefaultSSLVerify is the ssl_verify level of a server entry that leaves the
// key out. An explicit `ssl_verify: 0` still means SSLVerifyNone.
const DefaultSSLVerify = SSLVerifyHostname

// defaultServerConfig is the value a server entry holds before any of its
// keys are decoded, so it defines what each omitted key means. Both decoders
// start every entry from it: decodeServers (YAML) and
// ServerConfig.UnmarshalJSON (set_config).
func defaultServerConfig() ServerConfig {
	return ServerConfig{SSLVerify: DefaultSSLVerify}
}

// UnmarshalJSON decodes a server entry onto defaultServerConfig, so a key the
// JSON leaves out keeps its default instead of the zero value.
func (s *ServerConfig) UnmarshalJSON(b []byte) error {
	type plain ServerConfig // has no UnmarshalJSON, so the decode below does not recurse
	p := plain(defaultServerConfig())
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = ServerConfig(p)
	return nil
}

// decodeServers decodes the YAML document's servers sequence with each entry
// started from defaultServerConfig. It returns nil when the document has no
// servers sequence.
//
// It is a second pass over b, after decode's strict pass has reported unknown
// keys and type errors. The entries cannot be defaulted inside that pass:
// yaml.v3 decodes each sequence element onto a fresh zero value, and a
// custom UnmarshalYAML on ServerConfig would either lose the decoder's
// KnownFields setting (Node.Decode) or drop the whole entry from the slice
// when it returns an error (the func-style Unmarshaler).
func decodeServers(b []byte) ([]ServerConfig, error) {
	var raw struct {
		Servers []yaml.Node `yaml:"servers"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if raw.Servers == nil {
		return nil, nil
	}
	servers := make([]ServerConfig, len(raw.Servers))
	for i := range raw.Servers {
		servers[i] = defaultServerConfig()
		if err := raw.Servers[i].Decode(&servers[i]); err != nil {
			return nil, err
		}
	}
	return servers, nil
}
