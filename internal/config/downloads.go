package config

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
	// MinFreeSpaceCleanup is the free-space target after post-processing
	// cleanup completes. Must be >= MinFreeSpace.
	MinFreeSpaceCleanup ByteSize `yaml:"min_free_space_cleanup" json:"min_free_space_cleanup"`

	// ArticleCacheSize is the in-memory article cache budget. Capped at
	// constants.MaxArticleCacheBytes.
	ArticleCacheSize ByteSize `yaml:"article_cache_size" json:"article_cache_size"`

	// MaxArtTries is the per-article attempt count across all servers
	// before the article is marked bad. Must be >= 1.
	MaxArtTries int `yaml:"max_art_tries" json:"max_art_tries"`
	// MaxArtOpt is the per-article attempt count on optional (backup)
	// servers specifically. Must be >= 0.
	MaxArtOpt int `yaml:"max_art_opt" json:"max_art_opt"`

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
}
