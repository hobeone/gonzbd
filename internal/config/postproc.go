package config

// PostProcConfig controls archive extraction, par2 repair, and related
// post-download behavior. See spec §9.4.
type PostProcConfig struct {
	// EnableUnrar enables RAR extraction.
	EnableUnrar bool `yaml:"enable_unrar" json:"enable_unrar"`
	// Enable7zip enables 7z extraction.
	Enable7zip bool `yaml:"enable_7zip" json:"enable_7zip"`
	// EnableParCleanup deletes par2 files after a successful repair.
	EnableParCleanup bool `yaml:"enable_par_cleanup" json:"enable_par_cleanup"`
	// EnableRarCleanup deletes source archive files (RAR, 7z, split)
	// after a successful extraction. Mirrors Python SABnzbd's behavior
	// where PP bit 2 (delete) removes originals.
	EnableRarCleanup bool `yaml:"enable_rar_cleanup" json:"enable_rar_cleanup"`

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
	// Prefer7zip uses 7z instead of unrar for RAR extraction even when
	// unrar is available. 7z often handles edge-case RARs more reliably.
	Prefer7zip bool `yaml:"prefer_7zip" json:"prefer_7zip"`
	// FlatUnpack writes all extracted files to the job root, ignoring
	// archive-internal directories.
	FlatUnpack bool `yaml:"flat_unpack" json:"flat_unpack"`

	// DeobfuscateFilenames renames obfuscated files (random hex/UUIDs)
	// to use the job name as a base. Defaults to true.
	DeobfuscateFilenames bool `yaml:"deobfuscate_filenames" json:"deobfuscate_filenames"`

	// IgnoreSamples deletes files matching the SABnzbd sample/proof
	// regex (case-insensitive `(sample|proof)` with a word boundary)
	// after a successful unpack. Default false; mirrors Python
	// SABnzbd's misc.ignore_samples option.
	IgnoreSamples bool `yaml:"ignore_samples" json:"ignore_samples"`

	// CleanupExtensions is a list of file extensions (without leading
	// dot) to delete from the job directory after a successful unpack.
	// Common values: "nfo", "txt", "url", "srr", "sfv", "nzb".
	// Mirrors SABnzbd's cfg.cleanup_list() feature.
	CleanupExtensions []string `yaml:"cleanup_extensions" json:"cleanup_extensions"`

	// FolderRename enables the _UNPACK_/_FAILED_ prefix on the
	// destination directory during post-processing. While processing,
	// the folder is named "_UNPACK_<name>"; on success it becomes
	// "<name>"; on failure it becomes "_FAILED_<name>". This prevents
	// media managers (Sonarr, Plex) from importing incomplete downloads.
	// Mirrors SABnzbd's cfg.folder_rename(). Default false.
	FolderRename bool `yaml:"folder_rename" json:"folder_rename"`
}
