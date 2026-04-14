package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Validate checks the configuration for required-field, range, and
// uniqueness errors. All discovered problems are joined into a single
// error so the user can fix the file in one pass instead of edit-load
// looping over them.
//
// Validation does not touch the filesystem (no path-existence checks),
// because Load runs before subsystems are initialized and missing
// directories are auto-created at startup. Subsystems perform their own
// startup checks against the directories they need.
func (c *Config) Validate() error {
	var errs []error

	if err := c.General.validate(); err != nil {
		errs = append(errs, fmt.Errorf("general: %w", err))
	}
	if err := c.Downloads.validate(); err != nil {
		errs = append(errs, fmt.Errorf("downloads: %w", err))
	}
	if err := c.PostProc.validate(); err != nil {
		errs = append(errs, fmt.Errorf("postproc: %w", err))
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

	if err := validateUniqueNames("sorter", sorterNames(c.Sorters)); err != nil {
		errs = append(errs, err)
	}
	for i := range c.Sorters {
		if err := c.Sorters[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("sorters[%d] (%q): %w", i, c.Sorters[i].Name, err))
		}
	}

	if err := validateUniqueNames("schedule", scheduleNames(c.Schedules)); err != nil {
		errs = append(errs, err)
	}
	for i := range c.Schedules {
		if err := c.Schedules[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("schedules[%d] (%q): %w", i, c.Schedules[i].Name, err))
		}
	}

	if err := validateUniqueNames("rss feed", feedNames(c.RSS)); err != nil {
		errs = append(errs, err)
	}
	for i := range c.RSS {
		if err := c.RSS[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("rss[%d] (%q): %w", i, c.RSS[i].Name, err))
		}
	}

	return errors.Join(errs...)
}

var (
	apiKeyPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	cronTokenRE   = regexp.MustCompile(`^(\*|[0-9,\-]+)$`)
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
		errs = append(errs, fmt.Errorf("api_key: must be 16 lowercase hex characters; regenerate via `sabnzbd init`"))
	}
	if !apiKeyPattern.MatchString(g.NZBKey) {
		errs = append(errs, fmt.Errorf("nzb_key: must be 16 lowercase hex characters; regenerate via `sabnzbd init`"))
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
	if d.MinFreeSpaceCleanup < d.MinFreeSpace {
		errs = append(errs, fmt.Errorf("min_free_space_cleanup (%s) < min_free_space (%s)",
			d.MinFreeSpaceCleanup, d.MinFreeSpace))
	}
	if d.ArticleCacheSize < 0 {
		errs = append(errs, fmt.Errorf("article_cache_size: %d is negative", d.ArticleCacheSize))
	}
	if err := positive("max_art_tries", d.MaxArtTries); err != nil {
		errs = append(errs, err)
	}
	if err := nonNegative("max_art_opt", d.MaxArtOpt); err != nil {
		errs = append(errs, err)
	}
	if err := nonNegative("propagation_delay", d.PropagationDelay); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *PostProcConfig) validate() error {
	if strings.TrimSpace(p.Par2Command) == "" {
		return fmt.Errorf("par2_command: %w", errEmpty)
	}
	return nil
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
	if err := s.SSLVerify.validate(); err != nil {
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
	if c.PP < 0 || c.PP > 7 {
		return fmt.Errorf("pp: %d outside [0,7]", c.PP)
	}
	return nil
}

func (s *SorterConfig) validate() error {
	if s.MinSize < 0 {
		return fmt.Errorf("min_size: %d is negative", s.MinSize)
	}
	return nil
}

func (s *ScheduleConfig) validate() error {
	var errs []error
	if strings.TrimSpace(s.Action) == "" {
		errs = append(errs, fmt.Errorf("action: %w", errEmpty))
	}
	if !cronTokenRE.MatchString(s.Minute) {
		errs = append(errs, fmt.Errorf("minute: %q is not a valid cron token", s.Minute))
	}
	if !cronTokenRE.MatchString(s.Hour) {
		errs = append(errs, fmt.Errorf("hour: %q is not a valid cron token", s.Hour))
	}
	if !cronTokenRE.MatchString(s.DayOfWeek) {
		errs = append(errs, fmt.Errorf("dayofweek: %q is not a valid cron token", s.DayOfWeek))
	}
	return errors.Join(errs...)
}

func (f *RSSFeedConfig) validate() error {
	var errs []error
	if strings.TrimSpace(f.URI) == "" {
		errs = append(errs, fmt.Errorf("uri: %w", errEmpty))
	}
	for i := range f.Filters {
		if err := f.Filters[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("filters[%d] (%q): %w", i, f.Filters[i].Name, err))
		}
	}
	return errors.Join(errs...)
}

func (f *RSSFilterConfig) validate() error {
	var errs []error
	if f.Title != "" {
		if _, err := regexp.Compile(f.Title); err != nil {
			errs = append(errs, fmt.Errorf("title regex: %w", err))
		}
	}
	if f.Body != "" {
		if _, err := regexp.Compile(f.Body); err != nil {
			errs = append(errs, fmt.Errorf("body regex: %w", err))
		}
	}
	switch f.Type {
	case "", "require", "must_not_match", "ignore":
	default:
		errs = append(errs, fmt.Errorf("type: %q must be require, must_not_match, or ignore", f.Type))
	}
	if f.SizeFrom < 0 {
		errs = append(errs, fmt.Errorf("size_from: %d is negative", f.SizeFrom))
	}
	if f.SizeTo < 0 {
		errs = append(errs, fmt.Errorf("size_to: %d is negative", f.SizeTo))
	}
	if f.SizeFrom > 0 && f.SizeTo > 0 && f.SizeFrom > f.SizeTo {
		errs = append(errs, fmt.Errorf("size_from (%s) > size_to (%s)", f.SizeFrom, f.SizeTo))
	}
	if err := nonNegative("age", f.Age); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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

func sorterNames(s []SorterConfig) []string {
	names := make([]string, len(s))
	for i := range s {
		names[i] = s[i].Name
	}
	return names
}

func scheduleNames(s []ScheduleConfig) []string {
	names := make([]string, len(s))
	for i := range s {
		names[i] = s[i].Name
	}
	return names
}

func feedNames(f []RSSFeedConfig) []string {
	names := make([]string, len(f))
	for i := range f {
		names[i] = f[i].Name
	}
	return names
}
