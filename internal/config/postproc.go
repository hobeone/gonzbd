package config

// PostProcConfig controls archive extraction, par2 repair, and related
// post-download behavior. See spec §9.4.
type PostProcConfig struct {
	// EnableUnrar enables RAR extraction.
	EnableUnrar bool `yaml:"enable_unrar" json:"enable_unrar"`
	// Enable7zip enables 7z extraction.
	Enable7zip bool `yaml:"enable_7zip" json:"enable_7zip"`
	// DirectUnpack starts extraction while the download is still in
	// progress, when feasible.
	DirectUnpack bool `yaml:"direct_unpack" json:"direct_unpack"`
	// EnableParCleanup deletes par2 files after a successful repair.
	EnableParCleanup bool `yaml:"enable_par_cleanup" json:"enable_par_cleanup"`

	// Par2Command is the path to the par2 binary. May be a bare
	// executable name resolved via PATH.
	Par2Command string `yaml:"par2_command" json:"par2_command"`
	// UnrarCommand is the path to the unrar binary. Empty triggers
	// auto-detection at startup.
	UnrarCommand string `yaml:"unrar_command" json:"unrar_command"`
	// SevenzCommand is the path to the 7z binary. Empty triggers
	// auto-detection at startup.
	SevenzCommand string `yaml:"sevenz_command" json:"sevenz_command"`

	// Par2Turbo selects par2cmdline-turbo invocation arguments when
	// the binary supports them.
	Par2Turbo bool `yaml:"par2_turbo" json:"par2_turbo"`
	// IgnoreUnrarDates discards in-archive timestamps and uses the
	// extraction time instead.
	IgnoreUnrarDates bool `yaml:"ignore_unrar_dates" json:"ignore_unrar_dates"`
	// OverwriteFiles allows extraction to clobber existing files in
	// the destination.
	OverwriteFiles bool `yaml:"overwrite_files" json:"overwrite_files"`
	// FlatUnpack writes all extracted files to the job root, ignoring
	// archive-internal directories.
	FlatUnpack bool `yaml:"flat_unpack" json:"flat_unpack"`
}
