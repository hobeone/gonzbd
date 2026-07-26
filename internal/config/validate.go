package config

import (
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// Validate checks the configuration for required-field, range, and
// uniqueness errors. All discovered problems are joined into a single
// error so the user can fix the file in one pass instead of edit-load
// looping over them. Uses runtime.GOOS by default; see ValidateWithOS.
//
// Validation does not touch the filesystem (no path-existence checks),
// because Load runs before subsystems are initialized and missing
// directories are auto-created at startup. Subsystems perform their own
// startup checks against the directories they need.
func (c *Config) Validate() error {
	return c.ValidateWithOS(runtime.GOOS)
}

// ValidateWithOS checks the configuration using the specified operating
// system rather than runtime.GOOS. Exists so platform-specific checks
// (e.g. strict_sandbox's Linux-only constraint) can be exercised with
// t.Parallel() from tests without mutating shared package state.
func (c *Config) ValidateWithOS(goos string) error {
	var errs []error

	if err := c.General.validate(); err != nil {
		errs = append(errs, fmt.Errorf("general: %w", err))
	}
	if err := c.Downloads.validate(); err != nil {
		errs = append(errs, fmt.Errorf("downloads: %w", err))
	}
	if err := c.PostProc.validateWithOS(goos); err != nil {
		errs = append(errs, fmt.Errorf("postproc: %w", err))
	}
	if err := c.Notifications.validate(); err != nil {
		errs = append(errs, fmt.Errorf("notifications: %w", err))
	}

	if err := validateUniqueNames("server", serverNames(c.Servers)); err != nil {
		errs = append(errs, err)
	}
	for i := range c.Servers {
		if err := c.Servers[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("servers[%d] (%q): %w", i, c.Servers[i].Name, err))
		}
	}

	if err := validateUniqueNames("category", categoryNames(c.Categories)); err != nil {
		errs = append(errs, err)
	}
	for i := range c.Categories {
		if err := c.Categories[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("categories[%d] (%q): %w", i, c.Categories[i].Name, err))
		}
	}

	return errors.Join(errs...)
}

var (
	apiKeyPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// validateUniqueNames returns an error if any name appears more than once.
// Empty names are reported as a separate "must not be empty" error.
func validateUniqueNames(kind string, names []string) error {
	seen := make(map[string]int, len(names))
	var errs []error
	for i, n := range names {
		if n == "" {
			errs = append(errs, fmt.Errorf("%s[%d]: name %w", kind, i, errEmpty))
			continue
		}
		if prev, dup := seen[n]; dup {
			errs = append(errs, fmt.Errorf("%s name %q appears at indices %d and %d", kind, n, prev, i))
			continue
		}
		seen[n] = i
	}
	return errors.Join(errs...)
}

// nonNegative returns an error when v is negative.
func nonNegative(field string, v int) error {
	if v < 0 {
		return fmt.Errorf("%s: %d is negative", field, v)
	}
	return nil
}

// positive returns an error when v is not strictly positive.
func positive(field string, v int) error {
	if v <= 0 {
		return fmt.Errorf("%s: %d must be > 0", field, v)
	}
	return nil
}

// portInRange returns an error when p is outside the TCP port range.
// Zero is permitted and indicates "disabled" by convention; callers that
// require a real listener should check separately.
func portInRange(field string, p int, allowZero bool) error {
	if p == 0 {
		if allowZero {
			return nil
		}
		return fmt.Errorf("%s: 0 is not allowed", field)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s: %d outside [1,65535]", field, p)
	}
	return nil
}

func (g *GeneralConfig) validate() error {
	var errs []error

	if strings.TrimSpace(g.Host) == "" {
		errs = append(errs, fmt.Errorf("host: %w", errEmpty))
	}
	if err := portInRange("port", g.Port, false); err != nil {
		errs = append(errs, err)
	}
	if err := portInRange("https_port", g.HTTPSPort, true); err != nil {
		errs = append(errs, err)
	}
	if g.HTTPSPort > 0 {
		if g.HTTPSCert == "" {
			errs = append(errs, fmt.Errorf("https_cert: %w (required when https_port > 0)", errEmpty))
		}
		if g.HTTPSKey == "" {
			errs = append(errs, fmt.Errorf("https_key: %w (required when https_port > 0)", errEmpty))
		}
	}
	if !apiKeyPattern.MatchString(g.APIKey) {
		errs = append(errs, fmt.Errorf("api_key: must be 16 lowercase hex characters; regenerate via `gonzbd init`"))
	}
	if !apiKeyPattern.MatchString(g.NZBKey) {
		errs = append(errs, fmt.Errorf("nzb_key: must be 16 lowercase hex characters; regenerate via `gonzbd init`"))
	}
	if strings.TrimSpace(g.DownloadDir) == "" {
		errs = append(errs, fmt.Errorf("download_dir: %w", errEmpty))
	}
	if strings.TrimSpace(g.CompleteDir) == "" {
		errs = append(errs, fmt.Errorf("complete_dir: %w", errEmpty))
	}
	if g.DirscanDir != "" {
		if err := positive("dirscan_speed", g.DirscanSpeed); err != nil {
			errs = append(errs, err)
		}
	}
	// Validate per-component log level overrides at load time.
	if _, err := g.ParseLogLevels(); err != nil {
		errs = append(errs, fmt.Errorf("log_levels: %w", err))
	}
	// Validate trusted client ranges at load time so a typo fails fast
	// instead of silently locking out the reverse proxy / LAN.
	if _, err := ParseLocalRanges(g.LocalRanges); err != nil {
		errs = append(errs, fmt.Errorf("local_ranges: %w", err))
	}
	if !g.TrustedForwardHeader.Valid() {
		errs = append(errs, fmt.Errorf("trusted_forward_header: %q is not one of \"\", %q, %q, %q",
			g.TrustedForwardHeader, ForwardHeaderXFF, ForwardHeaderForwarded, ForwardHeaderXRealIP))
	}
	return errors.Join(errs...)
}

func (d *DownloadConfig) validate() error {
	var errs []error
	if d.BandwidthMax < 0 {
		errs = append(errs, fmt.Errorf("bandwidth_max: %d is negative", d.BandwidthMax))
	}
	if d.BandwidthPerc < 0 || d.BandwidthPerc > 100 {
		errs = append(errs, fmt.Errorf("bandwidth_perc: %d outside [0,100]", d.BandwidthPerc))
	}
	if d.MinFreeSpace < 0 {
		errs = append(errs, fmt.Errorf("min_free_space: %d is negative", d.MinFreeSpace))
	}
	if err := positive("max_art_tries", d.MaxArtTries); err != nil {
		errs = append(errs, err)
	}
	if err := nonNegative("max_art_opt", d.MaxArtOpt); err != nil {
		errs = append(errs, err)
	}
	if err := positive("max_active_jobs", d.MaxActiveJobs); err != nil {
		errs = append(errs, err)
	}
	if err := nonNegative("propagation_delay", d.PropagationDelay); err != nil {
		errs = append(errs, err)
	}
	if err := validateCleanupList(d.CleanupList); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

const (
	// MinPar2PacketBodySize is the minimum allowed non-zero value for Par2MaxPacketBodySize (64 KiB).
	MinPar2PacketBodySize uint64 = 65536
	// MinPar2JunkScanBytes is the minimum allowed non-zero value for Par2MaxJunkScanBytes (1 KiB).
	MinPar2JunkScanBytes int64 = 1024
)

func (p *PostProcConfig) validate() error {
	return p.validateWithOS(runtime.GOOS)
}

func (p *PostProcConfig) validateWithOS(goos string) error {
	var errs []error

	if p.StrictSandbox && goos != "linux" {
		errs = append(errs, fmt.Errorf("strict_sandbox is not supported on %s (only linux); disable strict_sandbox or run on linux", goos))
	}

	if p.Par2MaxPacketBodySize != 0 && p.Par2MaxPacketBodySize < MinPar2PacketBodySize {
		errs = append(errs, fmt.Errorf("par2_max_packet_body_size (%d) must be at least %d bytes (64 KiB)", p.Par2MaxPacketBodySize, MinPar2PacketBodySize))
	}
	if p.Par2MaxJunkScanBytes != 0 && p.Par2MaxJunkScanBytes < MinPar2JunkScanBytes {
		errs = append(errs, fmt.Errorf("par2_max_junk_scan_bytes (%d) must be at least %d bytes (1 KiB)", p.Par2MaxJunkScanBytes, MinPar2JunkScanBytes))
	}

	// Permissions must be valid 3- or 4-digit octal if set.
	if p.Permissions != "" {
		if len(p.Permissions) < 3 || len(p.Permissions) > 4 {
			errs = append(errs, fmt.Errorf("permissions: %q must be 3 or 4 octal digits (e.g. \"755\")", p.Permissions))
		} else {
			for _, c := range p.Permissions {
				if c < '0' || c > '7' {
					errs = append(errs, fmt.Errorf("permissions: %q contains non-octal character %q", p.Permissions, c))
					break
				}
			}
		}
	}

	// Extra params must be valid flags (each token starts with '-'), and unrar
	// params must only use SABnzbd allowlisted prefixes (-mlp, -om*, -ri*).
	if args, err := cmdutil.ParseExtraParams(p.ExtraUnrarParams); err != nil {
		errs = append(errs, fmt.Errorf("extra_unrar_params: %w", err))
	} else if err := cmdutil.ValidateUnrarParams(args); err != nil {
		errs = append(errs, fmt.Errorf("extra_unrar_params: %w", err))
	}
	if err := validateExtraParams("extra_par2_params", p.ExtraPar2Params); err != nil {
		errs = append(errs, err)
	}

	if err := cmdutil.ValidatePriorityArgs("nice", p.Nice); err != nil {
		errs = append(errs, err)
	}
	if err := cmdutil.ValidatePriorityArgs("ionice", p.Ionice); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// validateExtraParams checks that every whitespace-separated token in s
// starts with '-'. This prevents accidental injection of non-flag
// arguments into external tool invocations.
func validateExtraParams(field, s string) error {
	if s == "" {
		return nil
	}
	for tok := range strings.FieldsSeq(s) {
		if !strings.HasPrefix(tok, "-") {
			return fmt.Errorf("%s: token %q does not start with '-'", field, tok)
		}
	}
	return nil
}

// validateCleanupList checks that every pattern in the cleanup list is a valid regular expression.
func validateCleanupList(patterns []string) error {
	var errs []error
	for i, pat := range patterns {
		if _, err := regexp.Compile(pat); err != nil {
			errs = append(errs, fmt.Errorf("cleanup_list[%d] (%q): %w", i, pat, err))
		}
	}
	return errors.Join(errs...)
}

func (s *ServerConfig) validate() error {
	var errs []error
	if strings.TrimSpace(s.Host) == "" {
		errs = append(errs, fmt.Errorf("host: %w", errEmpty))
	}
	if err := portInRange("port", s.Port, false); err != nil {
		errs = append(errs, err)
	}
	if err := positive("connections", s.Connections); err != nil {
		errs = append(errs, err)
	}
	if err := s.SSLVerify.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := nonNegative("priority", s.Priority); err != nil {
		errs = append(errs, err)
	}
	if err := nonNegative("retention", s.Retention); err != nil {
		errs = append(errs, err)
	}
	if err := positive("timeout", s.Timeout); err != nil {
		errs = append(errs, err)
	}
	if err := positive("pipelining_requests", s.PipeliningRequests); err != nil {
		errs = append(errs, err)
	}
	if s.Required && s.Optional {
		errs = append(errs, fmt.Errorf("required and optional cannot both be true"))
	}
	return errors.Join(errs...)
}

func (c *CategoryConfig) validate() error {
	if c.PP < 0 || c.PP > 3 {
		return fmt.Errorf("pp: %d outside [0,3] (0=download, 1=+repair, 2=+unpack, 3=+delete)", c.PP)
	}
	return nil
}

// Helpers extract the Name field from each subsection slice for the
// validateUniqueNames helper.

func serverNames(s []ServerConfig) []string {
	names := make([]string, len(s))
	for i := range s {
		names[i] = s[i].Name
	}
	return names
}

func categoryNames(c []CategoryConfig) []string {
	names := make([]string, len(c))
	for i := range c {
		names[i] = c[i].Name
	}
	return names
}

func (n *NotificationConfig) validate() error {
	var errs []error
	if n.Email.Enable {
		if err := n.Email.validate(); err != nil {
			errs = append(errs, fmt.Errorf("email: %w", err))
		}
	}
	if n.Apprise.Enable {
		if err := n.Apprise.validate(); err != nil {
			errs = append(errs, fmt.Errorf("apprise: %w", err))
		}
	}
	if n.Script.Enable {
		if err := n.Script.validate(); err != nil {
			errs = append(errs, fmt.Errorf("script: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (e *EmailNotificationConfig) validate() error {
	var errs []error
	if strings.TrimSpace(e.Host) == "" {
		errs = append(errs, fmt.Errorf("host: %w", errEmpty))
	}
	if err := portInRange("port", e.Port, false); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(e.From) == "" {
		errs = append(errs, fmt.Errorf("from: %w", errEmpty))
	}
	if len(e.To) == 0 {
		errs = append(errs, fmt.Errorf("to: %w", errEmpty))
	} else {
		for i, to := range e.To {
			if strings.TrimSpace(to) == "" {
				errs = append(errs, fmt.Errorf("to[%d]: %w", i, errEmpty))
			}
		}
	}
	if err := validateEvents(e.Events); err != nil {
		errs = append(errs, fmt.Errorf("events: %w", err))
	}
	return errors.Join(errs...)
}

func (a *AppriseNotificationConfig) validate() error {
	var errs []error
	if strings.TrimSpace(a.URL) == "" && strings.TrimSpace(a.ServiceURL) == "" {
		errs = append(errs, fmt.Errorf("url: must set at least one of url or service_url"))
	}
	if err := validateEvents(a.Events); err != nil {
		errs = append(errs, fmt.Errorf("events: %w", err))
	}
	return errors.Join(errs...)
}

func (s *ScriptNotificationConfig) validate() error {
	var errs []error
	if strings.TrimSpace(s.Path) == "" {
		errs = append(errs, fmt.Errorf("path: %w", errEmpty))
	}
	if err := nonNegative("timeout", s.Timeout); err != nil {
		errs = append(errs, err)
	}
	if err := validateEvents(s.Events); err != nil {
		errs = append(errs, fmt.Errorf("events: %w", err))
	}
	return errors.Join(errs...)
}

var validEvents = map[string]struct{}{
	"DownloadStarted":        {},
	"DownloadComplete":       {},
	"DownloadFailed":         {},
	"PostProcessingComplete": {},
	"PostProcessingFailed":   {},
	"DiskFull":               {},
	"QueueDone":              {},
	"Warning":                {},
	"Error":                  {},
}

func validateEvents(events []string) error {
	var errs []error
	for _, ev := range events {
		if _, ok := validEvents[ev]; !ok {
			errs = append(errs, fmt.Errorf("invalid event %q", ev))
		}
	}
	return errors.Join(errs...)
}
