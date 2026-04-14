package config

// GeneralConfig holds top-level daemon settings: HTTP listen, credentials,
// directory layout, and language. See spec §9.2.
type GeneralConfig struct {
	// Host is the HTTP bind address.
	Host string `yaml:"host"`
	// Port is the HTTP port. 1-65535.
	Port int `yaml:"port"`
	// HTTPSPort is the HTTPS port. 0 disables HTTPS.
	HTTPSPort int `yaml:"https_port"`
	// HTTPSCert is the TLS certificate file. Required when HTTPSPort > 0.
	HTTPSCert string `yaml:"https_cert"`
	// HTTPSKey is the TLS private key file. Required when HTTPSPort > 0.
	HTTPSKey string `yaml:"https_key"`

	// APIKey authenticates API requests. 16-character lowercase hex.
	APIKey string `yaml:"api_key"`
	// NZBKey authenticates NZB-upload API requests. 16-character lowercase hex.
	NZBKey string `yaml:"nzb_key"`
	// Username for the web UI. Empty disables basic auth.
	Username string `yaml:"username"`
	// Password for the web UI. Stored as a hashed value (format set by
	// the auth subsystem); plaintext is rejected at validation time
	// when the auth subsystem is wired up.
	Password string `yaml:"password"`

	// DownloadDir is the work-in-progress directory for incomplete
	// downloads. Created on startup if it does not exist.
	DownloadDir string `yaml:"download_dir"`
	// CompleteDir is the destination for completed jobs.
	CompleteDir string `yaml:"complete_dir"`
	// DirscanDir is the watched folder for drop-in NZB files. Empty
	// disables the scanner.
	DirscanDir string `yaml:"dirscan_dir"`
	// DirscanSpeed is the scan interval (seconds, > 0).
	DirscanSpeed int `yaml:"dirscan_speed"`
	// ScriptDir holds user post-processing scripts.
	ScriptDir string `yaml:"script_dir"`
	// EmailDir holds notification email templates.
	EmailDir string `yaml:"email_dir"`
	// LogDir holds the server's own log files.
	LogDir string `yaml:"log_dir"`
	// AdminDir holds queue / state files.
	AdminDir string `yaml:"admin_dir"`

	// Language is the BCP-47 (or shorter) UI language code.
	Language string `yaml:"language"`
}
