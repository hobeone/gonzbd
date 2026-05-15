package config

// PostProcConfig controls archive extraction, par2 repair, and related
// post-download behavior. See spec §9.4.
type PostProcConfig struct {
	// EnableUnrar enables RAR extraction.
	EnableUnrar bool `yaml:"enable_unrar" json:"enable_unrar"`
	// Enable7zip enables 7z extraction.
	Enable7zip bool `yaml:"enable_7zip" json:"enable_7zip"`
	// EnableFileJoin enables split file joining (.001/.002/...).
	// Matches SABnzbd's cfg.enable_filejoin(). Default true.
	EnableFileJoin bool `yaml:"enable_filejoin" json:"enable_filejoin"`
	// EnableTSJoin enables TS file joining (.ts.001/.ts.002/...).
	// Matches SABnzbd's cfg.enable_tsjoin(). Default true.
	EnableTSJoin bool `yaml:"enable_tsjoin" json:"enable_tsjoin"`
	// EnableRecursive enables recursive unpacking: newly extracted
	// archives are unpacked up to maxUnpackDepth (3) levels deep.
	// Matches SABnzbd's cfg.enable_recursive(). Default true.
	EnableRecursive bool `yaml:"enable_recursive" json:"enable_recursive"`
	// EnableParCleanup deletes par2 files after a successful repair.
	EnableParCleanup bool `yaml:"enable_par_cleanup" json:"enable_par_cleanup"`
	// EnableRarCleanup deletes source archive files (RAR, 7z, split)
	// after a successful extraction. Mirrors Python SABnzbd's behavior
	// where PP bit 2 (delete) removes originals.
	EnableRarCleanup bool `yaml:"enable_rar_cleanup" json:"enable_rar_cleanup"`

	// ProcessUnpackedPar2 when true scans extracted archives for par2
	// files and uses them for 16K-MD5 filename recovery. This helps
	// deobfuscate files from nested archives. Matches SABnzbd's
	// cfg.process_unpacked_par2(). Default true.
	ProcessUnpackedPar2 bool `yaml:"process_unpacked_par2" json:"process_unpacked_par2"`

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
	// IgnoreUnrarDates discards in-archive modification timestamps and
	// uses the extraction time instead. Passes -tsm- to unrar (matches
	// SABnzbd's behavior).
	IgnoreUnrarDates bool `yaml:"ignore_unrar_dates" json:"ignore_unrar_dates"`
	// OverwriteFiles allows extraction to clobber existing files in
	// the destination.
	OverwriteFiles bool `yaml:"overwrite_files" json:"overwrite_files"`
	// Prefer7zip uses 7z instead of unrar for RAR extraction even when
	// unrar is available. 7z often handles edge-case RARs more reliably.
	Prefer7zip bool `yaml:"prefer_7zip" json:"prefer_7zip"`
	// PreferGoRAR uses the pure-Go rardecode library for RAR extraction
	// instead of shelling out to unrar. No external binary required.
	// Falls back to subprocess unrar/7z when set to false or on error.
	// Default true.
	PreferGoRAR bool `yaml:"prefer_go_rar" json:"prefer_go_rar"`
	// FlatUnpack writes all extracted files to the job root, ignoring
	// archive-internal directories.
	FlatUnpack bool `yaml:"flat_unpack" json:"flat_unpack"`
	// DirectUnpack starts extraction while the download is still in
	// progress. Completed RAR volumes are fed to unrar as they arrive,
	// overlapping download and extraction I/O. Falls back to standard
	// extraction on any error. Requires enable_unrar=true and
	// prefer_7zip=false. Default false.
	DirectUnpack bool `yaml:"direct_unpack" json:"direct_unpack"`
	// DirectUnpackThreads limits the number of concurrent DirectUnpack
	// workers across all jobs. 0 means no limit (one per active job).
	// Default 3 (matching SABnzbd).
	DirectUnpackThreads int `yaml:"direct_unpack_threads" json:"direct_unpack_threads"`

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

	// Nice is the argument string for the nice(1) command, prepended to
	// all external tool invocations (unrar, 7z, par2). Example: "-n 15".
	// Matches SABnzbd's cfg.nice(). Empty disables nice wrapping.
	Nice string `yaml:"nice" json:"nice"`
	// Ionice is the argument string for the ionice(1) command, prepended
	// to all external tool invocations. Example: "-c2 -n4".
	// Matches SABnzbd's cfg.ionice(). Empty disables ionice wrapping.
	Ionice string `yaml:"ionice" json:"ionice"`

	// Permissions is an octal permission string (e.g. "755") applied
	// recursively to extracted files after successful unpacking.
	// Directories receive the full permission; files have execute bits
	// stripped (e.g. "755" → dirs=0755, files=0644).
	// Matches SABnzbd's cfg.permissions(). Empty disables chmod.
	Permissions string `yaml:"permissions" json:"permissions"`

	// PasswordFile is the path to a text file containing one password per
	// line. Blank lines and lines starting with # are skipped.
	// These passwords are appended after per-job passwords and tried in
	// order during extraction. Matches SABnzbd's cfg.password_file().
	// A warning is logged if the file contains >30 passwords.
	PasswordFile string `yaml:"password_file" json:"password_file"`

	// ExtraUnrarParams holds additional command-line arguments appended to
	// every unrar invocation. Validated at startup — only flags starting
	// with '-' are allowed. Example: "-sl100000" (skip files >100KB).
	// Matches SABnzbd's extra unrar parameters concept.
	ExtraUnrarParams string `yaml:"extra_unrar_params" json:"extra_unrar_params"`

	// ExtraPar2Params holds additional command-line arguments appended to
	// every par2 invocation. Validated at startup — only flags starting
	// with '-' are allowed. Example: "-m512" (limit memory to 512MB).
	// Matches SABnzbd's cfg.extra_par2_parameters().
	ExtraPar2Params string `yaml:"extra_par2_params" json:"extra_par2_params"`

	// ScriptCanFail when true causes non-zero script exit codes to be
	// logged but NOT treated as job failures. This matches SABnzbd's
	// cfg.script_can_fail() behavior. Scripts that return non-zero for
	// informational reasons (e.g. partial failure but still usable) won't
	// cause the entire job to be marked as failed. Default false.
	ScriptCanFail bool `yaml:"script_can_fail" json:"script_can_fail"`
}
