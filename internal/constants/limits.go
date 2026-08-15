package constants

import "time"

// Binary unit prefixes (IEC 60027-2). These mirror the GIGI/MEBI/KIBI
// floats in Python sabnzbd/constants.py, expressed as int64 byte counts so
// they can be used directly with len()/cap() and unit conversions without
// fp rounding.
const (
	KiB int64 = 1 << 10 // 1024
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
	TiB int64 = 1 << 40
)

// Write cache limits. The write coalescing cache buffers decoded articles
// in the assembler and flushes them as larger contiguous writes.
const (
	// DefaultWriteCacheBytes is the default assembler write coalescing
	// cache size. Buffered articles are flushed as larger contiguous
	// writes, reducing I/O syscall count. 64 MiB holds ~90 typical
	// articles (700KB each).
	DefaultWriteCacheBytes int64 = 64 * MiB
)

// Checkpoint barrier bounds. A barrier drains a job's buffered writes, fsyncs
// its open files, and only then commits what the process may claim about them,
// so these two together are the stated bound on how much downloaded work a
// power loss can cost (design invariant B1). The barrier fires on whichever
// arrives first.
const (
	// DefaultCheckpointInterval is the default time bound between barriers
	// for one job. Thirty seconds of re-fetched articles is the worst case a
	// power loss can cost on a slow link.
	DefaultCheckpointInterval = 30 * time.Second

	// DefaultCheckpointBytes is the default volume bound between barriers
	// for one job, for links fast enough that 30 seconds is a lot of data.
	DefaultCheckpointBytes int64 = 64 * MiB
)

// Assembler / decoder limits.
const (
	// MaxAssemblerQueue is the in-flight backpressure threshold for the
	// assembler queue. Python: DEF_MAX_ASSEMBLER_QUEUE = 12.
	MaxAssemblerQueue = 12

	// SoftAssemblerQueueLimit is the fraction of MaxAssemblerQueue at
	// which downstream throttling kicks in. Python:
	// SOFT_ASSEMBLER_QUEUE_LIMIT = 0.5.
	SoftAssemblerQueueLimit = 0.5

	// AssemblerTriggerPercentage is the cache-fill fraction at which a
	// file is handed to the assembler even before all its parts have
	// arrived. Python: ASSEMBLER_TRIGGER_PERCENTAGE = 0.05.
	AssemblerTriggerPercentage = 0.05

	// AssemblerDelayFactorDirectWrite multiplies the write interval when
	// direct-write mode is enabled, since direct-write batches are larger.
	// Python: ASSEMBLER_DELAY_FACTOR_DIRECT_WRITE = 1.5.
	AssemblerDelayFactorDirectWrite = 1.5

	// AssemblerWriteInterval is the base poll/flush cadence of the
	// assembler. Python: ASSEMBLER_WRITE_INTERVAL = 5.0.
	AssemblerWriteInterval = 5 * time.Second
)

// NNTP wire-protocol buffer limits.
const (
	// NNTPBufferSize is the per-connection read buffer size.
	// Python: NNTP_BUFFER_SIZE = 256 KiB.
	NNTPBufferSize = 256 * 1024

	// NNTPMaxBufferSize is the upper bound on a single per-connection
	// read buffer (e.g. for very large multi-line responses).
	// Python: NTTP_MAX_BUFFER_SIZE = 10 MiB.
	NNTPMaxBufferSize = 10 * 1024 * 1024

	// DefaultPipeliningRequests is the default number of in-flight NNTP
	// commands per connection. Python: DEF_PIPELINING_REQUESTS = 2.
	DefaultPipeliningRequests = 2
)

// Network defaults.
const (
	// DefaultNetworkingTimeout is the standard NNTP socket timeout.
	// Python: DEF_NETWORKING_TIMEOUT = 60.
	DefaultNetworkingTimeout = 60 * time.Second

	// DefaultNetworkingTestTimeout is the timeout used for connectivity
	// probes (e.g. server test in the UI).
	// Python: DEF_NETWORKING_TEST_TIMEOUT = 5.
	DefaultNetworkingTestTimeout = 5 * time.Second

	// DefaultNetworkingShortTimeout is a short-lived timeout used for
	// quick probes (e.g. address resolution race).
	// Python: DEF_NETWORKING_SHORT_TIMEOUT = 3.
	DefaultNetworkingShortTimeout = 3 * time.Second

	// DefaultDirScanRate is the directory-scanner poll interval.
	// Python: DEF_SCANRATE = 5.
	DefaultDirScanRate = 5 * time.Second
)

// Filesystem name limits. Some headroom is reserved on top of OS limits to
// allow append-suffixes such as "_UNPACK_" or ".1" / ".2" disambiguators.
const (
	// MaxFolderNameLen is the maximum length of a folder name created by
	// gonzbd. Python: DEF_FOLDER_MAX = 256 - 10.
	MaxFolderNameLen = 256 - 10

	// MaxFileNameLen is the maximum length of a file name created by
	// gonzbd. Python: DEF_FILE_MAX = 255 - 10.
	MaxFileNameLen = 255 - 10

	// MaxFileExtensionLen is the maximum length of a file extension
	// (including the dot) preserved when truncating long file names.
	// Python: DEX_FILE_EXTENSION_MAX = 20.
	MaxFileExtensionLen = 20
)

// Warning / error counters.
const (
	// MaxWarnings is the maximum number of warnings retained for the UI
	// "warnings" panel. Python: MAX_WARNINGS = 20.
	MaxWarnings = 20

	// MaxBadArticles is the bad-article threshold for marking a job as
	// failed mid-download. Python: MAX_BAD_ARTICLES = 5.
	MaxBadArticles = 5
)

// Persistence file naming. These are the file basenames written into the
// admin directory. Versioning lets the on-disk format evolve without
// mistaking older files for current ones.
const (
	// AdminDirName is the per-instance admin directory under the user's
	// data root. Python: DEF_ADMIN_DIR = "admin".
	AdminDirName = "admin"

	// JobAdminDirName is the per-job admin subdirectory.
	// Python: JOB_ADMIN = "__ADMIN__".
	JobAdminDirName = "__ADMIN__"

	// VerifiedFileName marks a job whose files have been par2-verified.
	VerifiedFileName = "__verified__"

	// RenamesFileName records original->renamed file mappings for a job.
	RenamesFileName = "__renames__"

	// AttribFileName stores per-folder attribute metadata for a job.
	AttribFileName = "gonzbd_attrib"
)
