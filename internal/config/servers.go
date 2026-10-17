package config

import (
	"encoding/json"
	"fmt"

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
	// either tried it or are otherwise ineligible. A server whose work
	// queue is merely full holds the article rather than passing it down.
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

// DefaultServerConfig is the value a server entry holds before any of its
// keys are decoded, so it defines what each omitted key means. Both decoders
// start every entry from it: decodeServers (YAML) and
// ServerConfig.UnmarshalJSON (set_config). test_server's request decoding
// starts from it too, then passes the result to Checked.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{SSLVerify: DefaultSSLVerify}
}

// normalize fills the values a server entry may leave zero and that depend
// on its other keys, so they cannot be set before decoding: a port of 0
// means 119, or 563 with SSL.
func (s *ServerConfig) normalize() {
	if s.Port == 0 {
		if s.SSL {
			s.Port = 563
		} else {
			s.Port = 119
		}
	}
}

// Server is a server entry that passed ServerConfig.Checked. nntp.Dial and
// downloader.NewServer take a Server rather than a ServerConfig, so what they
// dial has been normalized and validated and they keep no fallbacks of their
// own. Checked is the only writer of checked:
// `git grep -n 'checked[:] true' -- '*.go'` finds 1 line, its return. The
// zero value, which any package can write, holds no server, and Config
// panics on it.
type Server struct {
	cfg     ServerConfig
	checked bool
}

// Config returns the checked entry. It panics on a Server that Checked did
// not produce: that is a programmer error, and dialling the zeroes such a
// value holds would hang or fail far from the cause.
func (s Server) Config() ServerConfig {
	if !s.checked {
		panic("config: Server used without ServerConfig.Checked")
	}
	return s.cfg
}

// Checked returns s as Config.finish leaves a configured server, normalized
// and then validated, or the error validation reports. The API's test_server
// builds its server through it (internal/api/config.go testServerConfig), so
// the test dials what a saved entry with the same values would.
func (s ServerConfig) Checked() (Server, error) {
	s.normalize()
	if err := s.validate(); err != nil {
		return Server{}, err
	}
	return Server{cfg: s, checked: true}, nil
}

// CheckedServers returns every configured server through Checked, in order,
// or the first error. A Config that came through Load or Set holds only
// servers Checked accepts; one whose Servers were assigned directly (With)
// may not, and the error names the entry.
func (c *Config) CheckedServers() ([]Server, error) {
	servers := c.GetServers()
	out := make([]Server, len(servers))
	for i, sc := range servers {
		s, err := sc.Checked()
		if err != nil {
			return nil, fmt.Errorf("servers[%d] (%q): %w", i, sc.Name, err)
		}
		out[i] = s
	}
	return out, nil
}

// UnmarshalJSON decodes a server entry onto DefaultServerConfig, so a key the
// JSON leaves out keeps its default instead of the zero value.
func (s *ServerConfig) UnmarshalJSON(b []byte) error {
	type plain ServerConfig // has no UnmarshalJSON, so the decode below does not recurse
	p := plain(DefaultServerConfig())
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = ServerConfig(p)
	return nil
}

// decodeServers decodes the YAML document's servers sequence with each entry
// started from DefaultServerConfig. It returns nil when the document has no
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
		servers[i] = DefaultServerConfig()
		if err := raw.Servers[i].Decode(&servers[i]); err != nil {
			return nil, err
		}
	}
	return servers, nil
}
