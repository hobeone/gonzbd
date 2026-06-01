package config

import (
	"strings"
	"testing"
)

// ---------- portInRange ----------

func TestPortInRange_ZeroNotAllowed(t *testing.T) {
	t.Parallel()
	if err := portInRange("port", 0, false); err == nil {
		t.Error("expected error for zero port when allowZero=false")
	}
}

func TestPortInRange_ZeroAllowed(t *testing.T) {
	t.Parallel()
	if err := portInRange("https_port", 0, true); err != nil {
		t.Errorf("expected nil for zero port when allowZero=true, got %v", err)
	}
}

func TestPortInRange_MaxBoundary(t *testing.T) {
	t.Parallel()
	if err := portInRange("port", 65535, false); err != nil {
		t.Errorf("65535 should be valid, got %v", err)
	}
}

func TestPortInRange_OutOfRange(t *testing.T) {
	t.Parallel()
	if err := portInRange("port", 65536, false); err == nil {
		t.Error("expected error for port 65536")
	}
}

// ---------- positive / nonNegative ----------

func TestPositive_ZeroFails(t *testing.T) {
	t.Parallel()
	if err := positive("connections", 0); err == nil {
		t.Error("expected error for positive(0)")
	}
}

func TestPositive_OnePasses(t *testing.T) {
	t.Parallel()
	if err := positive("connections", 1); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNonNegative_NegativeFails(t *testing.T) {
	t.Parallel()
	if err := nonNegative("max_art_opt", -1); err == nil {
		t.Error("expected error for nonNegative(-1)")
	}
}

func TestNonNegative_ZeroPasses(t *testing.T) {
	t.Parallel()
	if err := nonNegative("max_art_opt", 0); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------- validateExtraParams ----------

func TestValidateExtraParams_EmptyStringPasses(t *testing.T) {
	t.Parallel()
	if err := validateExtraParams("extra_unrar_params", ""); err != nil {
		t.Errorf("empty string should pass, got %v", err)
	}
}

func TestValidateExtraParams_ValidFlagsPasses(t *testing.T) {
	t.Parallel()
	if err := validateExtraParams("extra_par2_params", "-v -q -N4"); err != nil {
		t.Errorf("all-flag string should pass, got %v", err)
	}
}

func TestValidateExtraParams_NonFlagTokenFails(t *testing.T) {
	t.Parallel()
	if err := validateExtraParams("extra_unrar_params", "-sl100000 badarg"); err == nil {
		t.Error("expected error for token without leading '-'")
	}
}

// ---------- validateUniqueNames ----------

func TestValidateUniqueNames_EmptyNameFails(t *testing.T) {
	t.Parallel()
	err := validateUniqueNames("server", []string{"primary", "", "backup"})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q should mention empty", err.Error())
	}
}

func TestValidateUniqueNames_DuplicateFails(t *testing.T) {
	t.Parallel()
	err := validateUniqueNames("server", []string{"news", "news"})
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !strings.Contains(err.Error(), "news") {
		t.Errorf("error %q should mention the duplicate name", err.Error())
	}
}

func TestValidateUniqueNames_UniquePass(t *testing.T) {
	t.Parallel()
	if err := validateUniqueNames("server", []string{"primary", "backup"}); err != nil {
		t.Errorf("unique names should pass, got %v", err)
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

// ---------- ScheduleConfig.validate ----------

func TestValidateSchedule_BadCronMinute(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Schedules = []ScheduleConfig{{
		Name:      "s",
		Action:    "pause",
		Minute:    "xx",
		Hour:      "*",
		DayOfWeek: "*",
	}}
	requireValidateError(t, cfg, "minute")
}

func TestValidateSchedule_EmptyAction(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Schedules = []ScheduleConfig{{
		Name:      "s",
		Action:    "",
		Minute:    "*",
		Hour:      "*",
		DayOfWeek: "*",
	}}
	requireValidateError(t, cfg, "action")
}

func TestValidateSchedule_ValidPassthrough(t *testing.T) {
	t.Parallel()
	cfg := mustDefault(t)
	cfg.Schedules = []ScheduleConfig{{
		Name:      "nightly",
		Action:    "pause",
		Minute:    "0",
		Hour:      "2",
		DayOfWeek: "*",
	}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid schedule: unexpected error %v", err)
	}
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
