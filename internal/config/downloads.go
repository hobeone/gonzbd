package config

import "github.com/hobeone/gonzbd/internal/fsutil"

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

	// WriteCacheSize is the memory budget for write coalescing in the
	// assembler. When positive, decoded articles are buffered in memory
	// and flushed as larger contiguous writes, reducing I/O syscall
	// count and improving sequential write patterns. Zero disables
	// coalescing (each article is written individually).
	WriteCacheSize ByteSize `yaml:"write_cache_size" json:"write_cache_size"`

	// MaxArtTries is the per-article attempt count across all servers
	// before the article is marked bad. Must be >= 1.
	MaxArtTries int `yaml:"max_art_tries" json:"max_art_tries"`
	// MaxArtOpt is the per-article attempt count on optional (backup)
	// servers specifically. Must be >= 0.
	MaxArtOpt int `yaml:"max_art_opt" json:"max_art_opt"`

	// MaxActiveJobs is the maximum number of jobs processing (downloading or
	// repairing/extracting) concurrently. Defaults to 4.
	MaxActiveJobs int `yaml:"max_active_jobs" json:"max_active_jobs"`

	// TopOnly restricts dispatch to the highest-priority server per
	// article (no fallback to backup servers).
	TopOnly bool `yaml:"top_only" json:"top_only"`
	// NoPenalties replaces normal server penalty durations with
	// constants.PenaltyShort, useful for testing.
	NoPenalties bool `yaml:"no_penalties" json:"no_penalties"`

	// PreCheck issues an NNTP STAT before BODY to confirm article
	// availability. Trades latency for fewer wasted bytes on missing
	// articles.
	PreCheck bool `yaml:"pre_check" json:"pre_check"`

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
