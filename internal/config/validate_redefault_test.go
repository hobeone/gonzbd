package config

import (
	"strconv"
	"strings"
	"testing"
)

// TestValidate_FieldsConsumersUsedToRedefault pins the checks #917 adds: each
// field here was accepted while a consumer silently replaced the value, so the
// refusal is what lets the consumer read the field as written.
func TestValidate_FieldsConsumersUsedToRedefault(t *testing.T) {
	type validateCase struct {
		name    string
		mutate  func(*Config)
		wantErr string // "" means the value is valid
	}
	validPrio := []int{2, 1, 0, -1, -2, -100}
	invalidPrio := []int{99, 3, -3, -4, -99, -128, 1000}
	cases := make([]validateCase, 0, 8+len(validPrio)+len(invalidPrio))
	cases = append(cases, []validateCase{
		{"admin_dir empty", func(c *Config) { c.General.AdminDir = "" }, "general: admin_dir: must not be empty"},
		{"admin_dir blank", func(c *Config) { c.General.AdminDir = "  " }, "general: admin_dir: must not be empty"},
		{"admin_dir relative", func(c *Config) { c.General.AdminDir = "admin" }, ""},

		{"max_compute_slots -5", func(c *Config) { c.Downloads.MaxComputeSlots = -5 }, "downloads: max_compute_slots: -5 must be > 0"},
		{"max_compute_slots 0", func(c *Config) { c.Downloads.MaxComputeSlots = 0 }, "downloads: max_compute_slots: 0 must be > 0"},
		{"max_compute_slots 1", func(c *Config) { c.Downloads.MaxComputeSlots = 1 }, ""},

		{"direct_unpack_threads -1", func(c *Config) { c.PostProc.DirectUnpackThreads = -1 }, "postproc: direct_unpack_threads: -1 is negative"},
		{"direct_unpack_threads 0 (no limit)", func(c *Config) { c.PostProc.DirectUnpackThreads = 0 }, ""},
	}...)
	// A category priority is valid when ResolvePriority would use it (Force,
	// High, Normal, Low, Paused) or it is Default (-100, inherit).
	for _, p := range validPrio {
		cases = append(cases, validateCase{"category priority " + strconv.Itoa(p),
			func(c *Config) { c.Categories[0].Priority = p }, ""})
	}
	for _, p := range invalidPrio {
		cases = append(cases, validateCase{"category priority " + strconv.Itoa(p),
			func(c *Config) { c.Categories[0].Priority = p },
			`categories[0] ("Default"): priority: ` + strconv.Itoa(p) + ` is not`})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Default()
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			tc.mutate(cfg)
			err = cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate refused a valid value: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate accepted it; want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate = %v; want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestSet_AdminDirExpandingToEmptyIsRefused covers the path admin_dir takes
// from set_config: the value is checked after it is expanded.
func TestSet_AdminDirExpandingToEmptyIsRefused(t *testing.T) {
	t.Setenv(unsetEnvVar, "")
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	before := cfg.General.AdminDir
	if err := cfg.Set("general", "admin_dir", "$"+unsetEnvVar); err == nil {
		t.Fatal("Set accepted an admin_dir that expands to empty")
	}
	if cfg.General.AdminDir != before {
		t.Fatalf("AdminDir = %q after a refused Set, want %q", cfg.General.AdminDir, before)
	}
}
