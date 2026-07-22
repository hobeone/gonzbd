package config

// PostProcConfig controls archive extraction, par2 repair, and related
// post-download behavior. See spec §9.4.
type PostProcConfig struct {
	// EnableUnrar enables RAR extraction.
	EnableUnrar bool `yaml:"enable_unrar" json:"enable_unrar"`
	// Enable7zip enables 7z extraction.
	Enable7zip bool `yaml:"enable_7zip" json:"enable_7zip"`
	// EnableTar enables plain .tar extraction (not compressed variants
	// like .tar.gz/.tgz). Matches SABnzbd's cfg.enable_tar(). Default true.
	EnableTar bool `yaml:"enable_tar" json:"enable_tar"`
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

	// Par2MaxPacketBodySize limits the maximum payload size (in bytes) of a
	// single PAR2 packet. Packets with bodies larger than this are rejected
	// to prevent memory exhaustion from malformed or malicious PAR2 files.
	// This is a safety limit with no "unlimited" setting: a value of 0 (or
	// any value below the 65536-byte / 64 KiB floor) falls back to the
	// default of 67108864 (64 MiB). To permit genuinely large recovery
	// packets, set an explicit larger value.
	Par2MaxPacketBodySize uint64 `yaml:"par2_max_packet_body_size" json:"par2_max_packet_body_size"`

	// Par2MaxJunkScanBytes limits how many bytes of unrecognized non-PAR2
	// header data can be scanned before giving up when searching for a valid
	// PAR2 packet header in a file. Prevents excessive CPU and I/O time spent
	// scanning large corrupt files. A value of 0 (or any value below the
	// 1024-byte / 1 KiB floor) falls back to the default of 65536 (64 KiB).
	Par2MaxJunkScanBytes int64 `yaml:"par2_max_junk_scan_bytes" json:"par2_max_junk_scan_bytes"`

	// IgnoreUnrarDates discards in-archive modification timestamps and
	// uses the extraction time instead. Passes -tsm- to unrar (matches
	// SABnzbd's behavior).
	IgnoreUnrarDates bool `yaml:"ignore_unrar_dates" json:"ignore_unrar_dates"`
	// OverwriteFiles allows extraction to clobber existing files in
	// the destination.
	OverwriteFiles bool `yaml:"overwrite_files" json:"overwrite_files"`
	// UseGoRAR uses the pure-Go rarengine library for RAR5 extraction
	// instead of shelling out to unrar. No external binary required.
	// When false, falls back to subprocess unrar.
	// Default true.
	UseGoRAR bool `yaml:"use_go_rar" json:"use_go_rar"`
	// UseGo7z uses the pure-Go bodgit/sevenzip library for 7z extraction
	// instead of shelling out to 7zz/7z. No external binary required.
	// When false, falls back to subprocess 7z.
	// Default true.
	UseGo7z bool `yaml:"use_go_7z" json:"use_go_7z"`
	// UseGoPar2 uses the native par2engine library for PAR2 verification
	// and repair instead of shelling out to par2/par2cmdline. No external
	// binary required. When true and native verification/repair fails,
	// falls back to the external par2 binary if available.
	// Default true.
	UseGoPar2 bool `yaml:"use_go_par2" json:"use_go_par2"`
	// EnableRarVolumeRecovery attempts to recover a fully obfuscated RAR5
	// volume set (no filename numbering clue at all, and no PAR2 protection
	// covering the volumes) by reading volume sequencing directly from each
	// file's RAR5 header, then renaming to canonical part-numbered names.
	// A no-op whenever normal filename-based RAR detection already finds
	// something, so safe to leave enabled.
	// Default true.
	EnableRarVolumeRecovery bool `yaml:"enable_rar_volume_recovery" json:"enable_rar_volume_recovery"`
	// GoRarFallback controls whether a failed pure-Go RAR extraction
	// automatically retries with the external unrar binary. Only relevant
	// when UseGoRAR is true. When false, a go_unrar failure is final.
	// Default true.
	GoRarFallback bool `yaml:"go_rar_fallback" json:"go_rar_fallback"`
	// Go7zFallback controls whether a failed pure-Go 7-Zip extraction
	// automatically retries with the external 7z binary. Only relevant
	// when UseGo7z is true. When false, a go_7z failure is final.
	// Default true.
	Go7zFallback bool `yaml:"go_7z_fallback" json:"go_7z_fallback"`
	// GoPar2Fallback controls whether a failed native par2 repair
	// automatically retries with the external par2 binary. Only relevant
	// when UseGoPar2 is true. When false, a go_par2 failure is final.
	// Default true.
	GoPar2Fallback bool `yaml:"go_par2_fallback" json:"go_par2_fallback"`
	// EnableQuickCheck enables the CRC-based pre-verification pass before
	// par2 repair. When all file CRCs match, the expensive par2 subprocess
	// is skipped. Disable to always run the full par2 verify/repair step.
	// Default true.
	EnableQuickCheck bool `yaml:"enable_quick_check" json:"enable_quick_check"`
	// FlatUnpack writes all extracted files to the job root, ignoring
	// archive-internal directories.
	FlatUnpack bool `yaml:"flat_unpack" json:"flat_unpack"`
	// DirectUnpack starts extraction while the download is still in
	// progress. Completed RAR volumes are fed to the extractor as they
	// arrive, overlapping download and extraction I/O. Falls back to
	// standard extraction on any error. Requires enable_unrar=true.
	// Default false.
	DirectUnpack bool `yaml:"direct_unpack" json:"direct_unpack"`
	// DirectUnpackThreads limits the number of concurrent DirectUnpack
	// workers across all jobs. 0 means no limit (one per active job).
	// Default 3 (matching SABnzbd).
	DirectUnpackThreads int `yaml:"direct_unpack_threads" json:"direct_unpack_threads"`

	// StrictSandbox determines if external unpacker execution must abort
	// immediately when OS-level sandboxing (bwrap on Linux — the only
	// supported platform) cannot be established. Setting this to true on
	// any other platform is rejected at startup and at live config reload,
	// since there is no working sandbox backend there. Defaults to true.
	StrictSandbox bool `yaml:"strict_sandbox" json:"strict_sandbox"`

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

	// RedactScriptSecrets when true replaces SAB_API_KEY and SAB_PASSWORD
	// with a placeholder ("**REDACTED**") in the post-processing script
	// environment. This is a log-hygiene measure that reduces accidental
	// secret leakage from scripts that log or transmit their full
	// environment — it does NOT provide a security boundary (scripts run
	// as the daemon user and can read gonzbd.yaml from disk).
	//
	// WARNING: Scripts that use SAB_API_KEY for API callbacks (e.g.
	// Sonarr/Radarr import hooks, nzbToMedia) will break with HTTP 401
	// if this is enabled. Default false preserves backward compatibility.
	RedactScriptSecrets bool `yaml:"redact_script_secrets" json:"redact_script_secrets"`
}
