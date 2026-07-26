package config

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ---------- portInRange ----------

func TestPortInRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		field     string
		val       int
		allowZero bool
		wantErr   bool
	}{
		{"zero not allowed", "port", 0, false, true},
		{"zero allowed", "https_port", 0, true, false},
		{"min boundary", "port", 1, false, false},
		{"max boundary", "port", 65535, false, false},
		{"out of range upper", "port", 65536, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := portInRange(tc.field, tc.val, tc.allowZero)
			if tc.wantErr && err == nil {
				t.Errorf("portInRange(%q, %d, %v) = nil, want error", tc.field, tc.val, tc.allowZero)
			} else if !tc.wantErr && err != nil {
				t.Errorf("portInRange(%q, %d, %v) = %v, want nil", tc.field, tc.val, tc.allowZero, err)
			}
		})
	}
}

// ---------- positive / nonNegative ----------

func TestNumericBounds(t *testing.T) {
	t.Parallel()

	t.Run("positive", func(t *testing.T) {
		t.Parallel()
		if err := positive("connections", 0); err == nil {
			t.Error("expected error for positive(0)")
		}
		if err := positive("connections", 1); err != nil {
			t.Errorf("unexpected error for positive(1): %v", err)
		}
	})

	t.Run("nonNegative", func(t *testing.T) {
		t.Parallel()
		if err := nonNegative("max_art_opt", -1); err == nil {
			t.Error("expected error for nonNegative(-1)")
		}
		if err := nonNegative("max_art_opt", 0); err != nil {
			t.Errorf("unexpected error for nonNegative(0): %v", err)
		}
	})
}

// ---------- validateExtraParams ----------

func TestValidateExtraParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		field   string
		params  string
		wantErr bool
	}{
		{"empty string", "extra_unrar_params", "", false},
		{"valid flags", "extra_par2_params", "-v -q -N4", false},
		{"non-flag token", "extra_unrar_params", "-sl100000 badarg", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateExtraParams(tc.field, tc.params)
			if tc.wantErr && err == nil {
				t.Errorf("validateExtraParams(%q, %q) = nil, want error", tc.field, tc.params)
			} else if !tc.wantErr && err != nil {
				t.Errorf("validateExtraParams(%q, %q) = %v, want nil", tc.field, tc.params, err)
			}
		})
	}

	t.Run("disallowed unrar flag in config", func(t *testing.T) {
		t.Parallel()
		cfg, err := Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		cfg.PostProc.ExtraUnrarParams = "-df"
		if err := cfg.Validate(); err == nil {
			t.Error("expected error for disallowed unrar flag -df")
		} else if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("expected error to mention 'not allowed', got %v", err)
		}
	})
}

// ---------- validateUniqueNames ----------

func TestValidateUniqueNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		items   []string
		wantErr bool
		wantSub string
	}{
		{"empty name fails", []string{"primary", "", "backup"}, true, "empty"},
		{"duplicate fails", []string{"news", "news"}, true, "news"},
		{"unique passes", []string{"primary", "backup"}, false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateUniqueNames("server", tc.items)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateUniqueNames = nil, want error containing %q", tc.wantSub)
				}
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("error %q should mention %q", err.Error(), tc.wantSub)
				}
			} else if err != nil {
				t.Errorf("validateUniqueNames = %v, want nil", err)
			}
		})
	}
}

// ---------- DownloadConfig.validate ----------

func TestValidateDownloads_NegativeBandwidthMax(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Downloads.BandwidthMax = -1
	requireValidateError(t, cfg, "bandwidth_max")
}

func TestValidateDownloads_BandwidthPercOutOfRange(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Downloads.BandwidthPerc = 101
	requireValidateError(t, cfg, "bandwidth_perc")
}

func TestValidateDownloads_MaxArtTriesZero(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Downloads.MaxArtTries = 0
	requireValidateError(t, cfg, "max_art_tries")
}

func TestValidateDownloads_NegativePropagationDelay(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Downloads.PropagationDelay = -1
	requireValidateError(t, cfg, "propagation_delay")
}

// ---------- ServerConfig.validate ----------

func TestValidateServer_RequiredAndOptionalConflict(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Servers = []ServerConfig{validServer("s1")}
	cfg.Servers[0].Required = true
	cfg.Servers[0].Optional = true
	requireValidateError(t, cfg, "required and optional")
}

func TestValidateServer_ZeroConnections(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Servers = []ServerConfig{validServer("s1")}
	cfg.Servers[0].Connections = 0
	requireValidateError(t, cfg, "connections")
}

func TestValidateServer_EmptyHost(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Servers = []ServerConfig{validServer("s1")}
	cfg.Servers[0].Host = ""
	requireValidateError(t, cfg, "host")
}

func TestValidateServer_DuplicateNames(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Servers = []ServerConfig{validServer("news"), validServer("news")}
	requireValidateError(t, cfg, "news")
}

// ---------- helpers ----------

func mustDefault(t *testing.T) *Config {
	t.Helper()
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	return cfg
}

func requireValidateError(t *testing.T, cfg *Config, wantSub string) {
	t.Helper()
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("Validate: expected error containing %q, got nil", wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Errorf("Validate error %q does not contain %q", err.Error(), wantSub)
	}
}

func validServer(name string) ServerConfig {
	return ServerConfig{
		Name:               name,
		Host:               "news.example.com",
		Port:               563,
		Connections:        8,
		SSL:                true,
		SSLVerify:          SSLVerifyHostname,
		Priority:           0,
		Timeout:            60,
		PipeliningRequests: 2,
		Enable:             true,
	}
}

// ---------- Direct Validation Helpers ----------

func TestGeneralConfig_ValidateDirect(t *testing.T) {
	t.Parallel()
	cleanG := GeneralConfig{
		Host:        "localhost",
		Port:        8080,
		DownloadDir: "/tmp/download",
		CompleteDir: "/tmp/complete",
		APIKey:      "abcdef0123456789",
		NZBKey:      "0123456789abcdef",
	}
	if err := cleanG.validate(); err != nil {
		t.Errorf("expected clean validate, got: %v", err)
	}

	g := cleanG
	g.APIKey = "short"
	if err := g.validate(); err == nil {
		t.Error("expected validation error for invalid API key")
	}

	g2 := cleanG
	g2.HTTPSPort = 65536
	g2.HTTPSCert = "cert.pem"
	g2.HTTPSKey = "key.pem"
	if err := g2.validate(); err == nil {
		t.Error("expected error for invalid HTTPSPort")
	}

	g3 := cleanG
	g3.HTTPSPort = 443
	g3.HTTPSCert = ""
	if err := g3.validate(); err == nil {
		t.Error("expected error for missing HTTPSCert")
	}

	g4 := cleanG
	g4.HTTPSPort = 443
	g4.HTTPSCert = "cert"
	g4.HTTPSKey = ""
	if err := g4.validate(); err == nil {
		t.Error("expected error for missing HTTPSKey")
	}

	g5 := cleanG
	g5.DirscanDir = "/tmp"
	g5.DirscanSpeed = 0
	if err := g5.validate(); err == nil {
		t.Error("expected error for zero dirscan_speed with dirscan_dir")
	}
}

func TestDownloadConfig_ValidateDirect(t *testing.T) {
	t.Parallel()
	d := DownloadConfig{
		BandwidthMax:  100,
		BandwidthPerc: 50,
		MaxArtTries:   3,
		MaxActiveJobs: 4,
	}
	if err := d.validate(); err != nil {
		t.Errorf("expected clean validate, got: %v", err)
	}

	d.BandwidthPerc = 0
	if err := d.validate(); err != nil {
		t.Errorf("0 percent should pass, got: %v", err)
	}

	d.BandwidthPerc = 100
	if err := d.validate(); err != nil {
		t.Errorf("100 percent should pass, got: %v", err)
	}

	d.BandwidthPerc = 50
	d.MaxArtOpt = -1
	if err := d.validate(); err == nil {
		t.Error("expected error for negative max_art_opt")
	}

	d.MaxArtOpt = 0
	d.MaxActiveJobs = 0
	if err := d.validate(); err == nil {
		t.Error("expected error for non-positive max_active_jobs")
	}

	d.MaxActiveJobs = 4
	d.BandwidthMax = -1
	if err := d.validate(); err == nil {
		t.Error("expected error for negative bandwidth_max")
	}
}

func TestPostProcConfig_ValidateDirect(t *testing.T) {
	t.Parallel()
	p := PostProcConfig{
		Permissions: "755",
	}
	if err := p.validate(); err != nil {
		t.Errorf("expected clean validate, got: %v", err)
	}

	p.Permissions = "0755"
	if err := p.validate(); err != nil {
		t.Errorf("4-digit permissions should pass, got: %v", err)
	}

	p.Permissions = "700"
	if err := p.validate(); err != nil {
		t.Errorf("permissions with 0 should pass, got: %v", err)
	}

	p.Permissions = "899"
	if err := p.validate(); err == nil {
		t.Error("expected error for non-octal permissions")
	}
}

func TestPostProcConfig_Par2LimitsValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		par2MaxPacketBodySize uint64
		par2MaxJunkScanBytes  int64
		wantErr               bool
		wantSub               string
	}{
		{
			name:                  "valid defaults",
			par2MaxPacketBodySize: 67108864,
			par2MaxJunkScanBytes:  65536,
			wantErr:               false,
		},
		{
			name:                  "valid zeros (unlimited)",
			par2MaxPacketBodySize: 0,
			par2MaxJunkScanBytes:  0,
			wantErr:               false,
		},
		{
			name:                  "valid exact floors",
			par2MaxPacketBodySize: MinPar2PacketBodySize,
			par2MaxJunkScanBytes:  MinPar2JunkScanBytes,
			wantErr:               false,
		},
		{
			name:                  "below packet body size floor",
			par2MaxPacketBodySize: MinPar2PacketBodySize - 1,
			par2MaxJunkScanBytes:  65536,
			wantErr:               true,
			wantSub:               "par2_max_packet_body_size (65535) must be at least 65536 bytes (64 KiB)",
		},
		{
			name:                  "below junk scan bytes floor",
			par2MaxPacketBodySize: 67108864,
			par2MaxJunkScanBytes:  MinPar2JunkScanBytes - 1,
			wantErr:               true,
			wantSub:               "par2_max_junk_scan_bytes (1023) must be at least 1024 bytes (1 KiB)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := PostProcConfig{
				Par2MaxPacketBodySize: tt.par2MaxPacketBodySize,
				Par2MaxJunkScanBytes:  tt.par2MaxJunkScanBytes,
			}
			err := p.validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantSub)
				}
				if !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantSub)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPostProcConfig_ValidatePriorityWrapper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		nice    string
		ionice  string
		wantErr bool
	}{
		{"valid nice", "-n 15", "", false},
		{"valid ionice", "", "-c2 -n4", false},
		{"valid both", "-n 15", "-c2 -n4", false},
		{"nice semicolon injection", "-n 15; rm -rf /", "", true},
		{"ionice semicolon injection", "", "-c2; rm -rf /", true},
		{"nice double quote injection", "-n \"15\"", "", true},
		{"nice backtick injection", "-n `whoami`", "", true},
		{"ionice pipe injection", "-c2 | cat", "", true},
		{"nice dollar injection", "-n $HOME", "", true},
		{"ionice ampersand", "-c2 &", "", true},
		{"nice parens", "(-n 15)", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := PostProcConfig{
				Nice:   tt.nice,
				Ionice: tt.ionice,
			}
			err := p.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("PostProcConfig.validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	t.Run("top level Config.Validate rejects injection", func(t *testing.T) {
		t.Parallel()
		c := &Config{}
		c.PostProc.Nice = "-n 15; rm -rf /"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "postproc") {
			t.Errorf("expected Config.Validate to return postproc error for nice injection, got %v", err)
		}
	})
}

func TestServerConfig_ValidateDirect(t *testing.T) {
	t.Parallel()
	s := ServerConfig{
		Host:               "news.example.com",
		Port:               119,
		Connections:        4,
		Timeout:            120,
		PipeliningRequests: 5,
	}
	if err := s.validate(); err != nil {
		t.Errorf("expected clean validate, got: %v", err)
	}

	s1 := s
	s1.Connections = 0
	if err := s1.validate(); err == nil {
		t.Error("expected error for 0 connections")
	}

	s2 := s
	s2.Port = 999999
	if err := s2.validate(); err == nil {
		t.Error("expected error for invalid Port")
	}

	s3 := s
	s3.SSLVerify = SSLVerify(9)
	if err := s3.validate(); err == nil {
		t.Error("expected error for invalid SSLVerify")
	}

	s4 := s
	s4.Priority = -1
	if err := s4.validate(); err == nil {
		t.Error("expected error for negative Priority")
	}

	s5 := s
	s5.Retention = -1
	if err := s5.validate(); err == nil {
		t.Error("expected error for negative Retention")
	}

	s6 := s
	s6.Timeout = 0
	if err := s6.validate(); err == nil {
		t.Error("expected error for zero Timeout")
	}

	s7 := s
	s7.PipeliningRequests = 0
	if err := s7.validate(); err == nil {
		t.Error("expected error for zero PipeliningRequests")
	}
}

func TestCategoryConfig_ValidateDirect(t *testing.T) {
	t.Parallel()
	c := CategoryConfig{
		PP: 2,
	}
	if err := c.validate(); err != nil {
		t.Errorf("expected clean validate, got: %v", err)
	}

	c.PP = 4
	if err := c.validate(); err == nil {
		t.Error("expected error for PP=4")
	}
}

func TestNamesHelpersDirect(t *testing.T) {
	t.Parallel()
	servers := []ServerConfig{{Name: "s1"}, {Name: "s2"}}
	categories := []CategoryConfig{{Name: "c1"}, {Name: "c2"}}

	if got := serverNames(servers); !slices.Equal(got, []string{"s1", "s2"}) {
		t.Errorf("serverNames = %v", got)
	}
	if got := categoryNames(categories); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("categoryNames = %v", got)
	}
}

func TestDownloadConfig_InvalidCleanupListRegex(t *testing.T) {
	t.Parallel()
	d := DownloadConfig{
		BandwidthMax:  100,
		BandwidthPerc: 50,
		MaxArtTries:   3,
		CleanupList:   []string{"[a-z", "valid-regex"}, // [a-z is invalid (missing closing bracket)
	}
	err := d.validate()
	if err == nil {
		t.Fatal("expected validation error for invalid cleanup_list regex, got nil")
	}
	if !strings.Contains(err.Error(), "cleanup_list") {
		t.Errorf("expected error to mention 'cleanup_list', got: %v", err)
	}
	if !strings.Contains(err.Error(), "error parsing regexp") {
		t.Errorf("expected error to contain regexp compilation error message, got: %v", err)
	}
}

// ---------- NotificationConfig.validate ----------

func TestNotificationConfig_ValidateDirect(t *testing.T) {
	t.Parallel()

	// Empty/Disabled notifications should pass.
	nEmpty := NotificationConfig{}
	if err := nEmpty.validate(); err != nil {
		t.Errorf("empty notifications config should pass, got: %v", err)
	}

	// Email Validations
	t.Run("Email", func(t *testing.T) {
		t.Parallel()

		// Valid Email Config
		n := NotificationConfig{
			Email: EmailNotificationConfig{
				Enable: true,
				Host:   "smtp.example.com",
				Port:   587,
				From:   "sender@example.com",
				To:     []string{"recipient@example.com"},
				Events: []string{"DownloadStarted", "DownloadComplete"},
			},
		}
		if err := n.validate(); err != nil {
			t.Errorf("valid email config should pass, got: %v", err)
		}

		// Email Host empty
		badHost := n
		badHost.Email.Host = ""
		if err := badHost.validate(); err == nil || !strings.Contains(err.Error(), "host") {
			t.Errorf("expected host validation error, got: %v", err)
		}

		// Email Port out of range
		badPort := n
		badPort.Email.Port = 0
		if err := badPort.validate(); err == nil || !strings.Contains(err.Error(), "port") {
			t.Errorf("expected port validation error, got: %v", err)
		}
		badPort.Email.Port = 65536
		if err := badPort.validate(); err == nil || !strings.Contains(err.Error(), "port") {
			t.Errorf("expected port validation error, got: %v", err)
		}

		// Email From empty
		badFrom := n
		badFrom.Email.From = ""
		if err := badFrom.validate(); err == nil || !strings.Contains(err.Error(), "from") {
			t.Errorf("expected from validation error, got: %v", err)
		}

		// Email To empty slice or empty strings
		badToSlice := n
		badToSlice.Email.To = []string{}
		if err := badToSlice.validate(); err == nil || !strings.Contains(err.Error(), "to") {
			t.Errorf("expected to validation error for empty slice, got: %v", err)
		}
		badToStrings := n
		badToStrings.Email.To = []string{"valid@example.com", "", "alsovalid@example.com"}
		if err := badToStrings.validate(); err == nil || !strings.Contains(err.Error(), "to") {
			t.Errorf("expected to validation error for empty string in slice, got: %v", err)
		}

		// Email Invalid Event
		badEvent := n
		badEvent.Email.Events = []string{"DownloadStarted", "InvalidEventName"}
		if err := badEvent.validate(); err == nil || !strings.Contains(err.Error(), "events") {
			t.Errorf("expected events validation error, got: %v", err)
		}
	})

	// Apprise Validations
	t.Run("Apprise", func(t *testing.T) {
		t.Parallel()

		// Valid Apprise with URL
		nURL := NotificationConfig{
			Apprise: AppriseNotificationConfig{
				Enable: true,
				URL:    "http://apprise.local",
				Events: []string{"DiskFull"},
			},
		}
		if err := nURL.validate(); err != nil {
			t.Errorf("valid apprise config with URL should pass, got: %v", err)
		}

		// Valid Apprise with ServiceURL
		nServiceURL := NotificationConfig{
			Apprise: AppriseNotificationConfig{
				Enable:     true,
				ServiceURL: "pushed://user@service",
				Events:     []string{"DiskFull"},
			},
		}
		if err := nServiceURL.validate(); err != nil {
			t.Errorf("valid apprise config with ServiceURL should pass, got: %v", err)
		}

		// Apprise URLs both empty
		badBothEmpty := NotificationConfig{
			Apprise: AppriseNotificationConfig{
				Enable: true,
			},
		}
		if err := badBothEmpty.validate(); err == nil || !strings.Contains(err.Error(), "url") {
			t.Errorf("expected apprise url/service_url validation error, got: %v", err)
		}

		// Apprise Invalid Event
		badEvent := nURL
		badEvent.Apprise.Events = []string{"InvalidEvent"}
		if err := badEvent.validate(); err == nil || !strings.Contains(err.Error(), "events") {
			t.Errorf("expected apprise events validation error, got: %v", err)
		}
	})

	// Script Validations
	t.Run("Script", func(t *testing.T) {
		t.Parallel()

		// Valid Script
		n := NotificationConfig{
			Script: ScriptNotificationConfig{
				Enable:  true,
				Path:    "/path/to/script.sh",
				Timeout: 10,
				Events:  []string{"QueueDone"},
			},
		}
		if err := n.validate(); err != nil {
			t.Errorf("valid script config should pass, got: %v", err)
		}

		// Script Path empty
		badPath := n
		badPath.Script.Path = ""
		if err := badPath.validate(); err == nil || !strings.Contains(err.Error(), "path") {
			t.Errorf("expected script path validation error, got: %v", err)
		}

		// Script Timeout negative
		badTimeout := n
		badTimeout.Script.Timeout = -1
		if err := badTimeout.validate(); err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Errorf("expected script timeout validation error, got: %v", err)
		}

		// Script Invalid Event
		badEvent := n
		badEvent.Script.Events = []string{"QueueDone", "InvalidEvent"}
		if err := badEvent.validate(); err == nil || !strings.Contains(err.Error(), "events") {
			t.Errorf("expected script events validation error, got: %v", err)
		}
	})
}

func TestConfigValidate_NotificationsWired(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Notifications.Email.Enable = true
	cfg.Notifications.Email.Host = "" // invalid email host
	requireValidateError(t, cfg, "notifications")
}

func TestPostProcConfigValidate_StrictSandbox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		platform string
		strict   bool
		wantErr  bool
	}{
		{"linux", true, false},
		{"linux", false, false},
		{"darwin", true, true},
		{"darwin", false, false},
		{"freebsd", true, true},
		{"freebsd", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.platform+"_strict_"+strconv.FormatBool(tc.strict), func(t *testing.T) {
			t.Parallel()
			p := PostProcConfig{StrictSandbox: tc.strict}
			err := p.validateWithOS(tc.platform)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for strict=%v on %s, got nil", tc.strict, tc.platform)
				} else if !strings.Contains(err.Error(), "strict_sandbox") {
					t.Errorf("expected error to mention 'strict_sandbox', got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for strict=%v on %s: %v", tc.strict, tc.platform, err)
				}
			}
		})
	}
}
