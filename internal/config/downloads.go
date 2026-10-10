package config

import (
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// DownloadConfig controls bandwidth, retry behavior, and disk-space
// guards for the download pipeline. See spec §9.3.
type DownloadConfig struct {
	// BandwidthMax is the upper bandwidth ceiling. 0 = unlimited.
	BandwidthMax ByteSize `yaml:"bandwidth_max" json:"bandwidth_max"`
	// BandwidthPerc is the percentage of BandwidthMax actually used.
	// 0-100. Defaults to 100.
	BandwidthPerc Percent `yaml:"bandwidth_perc" json:"bandwidth_perc"`

	// MinFreeSpace is the minimum free disk space on the download
	// volume. Below this the downloader pauses.
	MinFreeSpace ByteSize `yaml:"min_free_space" json:"min_free_space"`

	// MaxArtTries is the per-article attempt count across all servers
	// before the article is marked bad. Must be >= 1.
	MaxArtTries int `yaml:"max_art_tries" json:"max_art_tries"`
	// MaxArtOpt is the per-article attempt count on optional (backup)
	// servers specifically. Must be >= 0.
	MaxArtOpt int `yaml:"max_art_opt" json:"max_art_opt"`

	// MaxActiveJobs is the maximum number of jobs processing (downloading or
	// repairing/extracting) concurrently. Defaults to 4.
	MaxActiveJobs int `yaml:"max_active_jobs" json:"max_active_jobs"`

	// MaxComputeSlots is the maximum number of jobs concurrently performing
	// compute- or disk-heavy post-processing stages (Assessing, Repairing,
	// Extracting, Finalizing). Defaults to 2.
	MaxComputeSlots int `yaml:"max_compute_slots" json:"max_compute_slots"`

	// TopOnly restricts dispatch to the highest-priority server per
	// article (no fallback to backup servers).
	TopOnly bool `yaml:"top_only" json:"top_only"`
	// NoPenalties replaces normal server penalty durations with
	// constants.PenaltyShort, useful for testing.
	NoPenalties bool `yaml:"no_penalties" json:"no_penalties"`

	// OnDemandPar2 defers par2 recovery volumes (*.volNNN+MM.par2) and only
	// downloads them if CRC verification shows the download needs repair. The
	// par2 index file is always downloaded. Saves bandwidth on intact
	// downloads. Default: true.
	OnDemandPar2 bool `yaml:"on_demand_par2" json:"on_demand_par2"`

	// PropagationDelay is the minutes to wait after a job is added
	// before downloading begins, allowing articles to propagate to
	// backup servers. 0 disables.
	PropagationDelay int `yaml:"propagation_delay" json:"propagation_delay"`

	// ReplaceIllegalWith is the string used to replace illegal filesystem
	// characters (e.g. \/:*?"<>|). Defaults to "_".
	ReplaceIllegalWith string `yaml:"replace_illegal_with" json:"replace_illegal_with"`
	// ReplaceSpacesWith is the string used to replace spaces in folder and
	// filenames. Defaults to "" (keep spaces).
	ReplaceSpacesWith string `yaml:"replace_spaces_with" json:"replace_spaces_with"`

	// StripDiacritics, if true, will replace accented characters with their
	// ASCII equivalents (e.g. é -> e). Defaults to false.
	StripDiacritics bool `yaml:"strip_diacritics" json:"strip_diacritics"`

	// CleanupList is a list of strings or regex patterns to be removed
	// from folder and filenames.
	CleanupList []string `yaml:"cleanup_list" json:"cleanup_list"`

	// UnwantedExtensions lists file extensions (without the dot,
	// case-insensitive; path.Match patterns allowed) that a job must not
	// deliver. Read according to UnwantedExtensionsMode, and acted on
	// according to ActionOnUnwantedExtensions. Mirrors SABnzbd's
	// unwanted_extensions. Default: exe, com, scr, pif, bat, cmd, msi, vbs.
	UnwantedExtensions []string `yaml:"unwanted_extensions" json:"unwanted_extensions"`

	// UnwantedExtensionsMode is "blacklist" (the listed extensions are
	// unwanted) or "whitelist" (every extension not listed is unwanted). A
	// name with no extension is never unwanted. Default: blacklist.
	UnwantedExtensionsMode unwanted.Mode `yaml:"unwanted_extensions_mode" json:"unwanted_extensions_mode"`

	// ActionOnUnwantedExtensions is what happens when an NZB names an
	// unwanted file, or a RAR5 volume or par2 file downloaded for the job
	// does: "off" disables the check, "pause" pauses the job until the user
	// resumes it, "fail" files it in history as Failed (at add, before
	// anything downloads, or at the point a downloaded file names one).
	// Unless it is "off", unwanted files are
	// also deleted after unpack, except from a job the user approved.
	// Default: pause.
	ActionOnUnwantedExtensions unwanted.Action `yaml:"action_on_unwanted_extensions" json:"action_on_unwanted_extensions"`
}

// UnwantedRules returns the unwanted-extension rules these settings
// describe. Validate rejects settings it would refuse, so the error is
// reachable only for a DownloadConfig that was never validated.
func (c DownloadConfig) UnwantedRules() (unwanted.Rules, error) {
	return unwanted.NewRules(c.ActionOnUnwantedExtensions, c.UnwantedExtensionsMode, c.UnwantedExtensions)
}

// SanitizeOptions returns the naming and cleanup preferences as a
// consolidated options struct.
func (c DownloadConfig) SanitizeOptions() fsutil.SanitizeOptions {
	return fsutil.SanitizeOptions{
		ReplaceIllegalWith: c.ReplaceIllegalWith,
		ReplaceSpacesWith:  c.ReplaceSpacesWith,
		StripDiacritics:    c.StripDiacritics,
		CleanupList:        c.CleanupList,
		CleanupRegexps:     fsutil.CompileCleanupList(c.CleanupList),
	}
}
