// Package queue implements the in-memory job queue and its on-disk
// persistence.
//
// The queue is the central coordination point for the daemon: the HTTP
// layer calls Add/Remove/Pause/Resume; the downloader selects over
// Notify() and reads jobs in order via List(); the persistence layer
// serialises state to the admin directory so a restart can recover in
// flight downloads.
//
// # Concurrency model
//
// All public methods on *Queue are safe for concurrent use. Read-heavy
// operations (List, Len, Get) take the read lock; structural mutations
// (Add, Remove, Reorder, status changes) take the write lock.
//
// Jobs returned from List/Get are shared references into the Queue's
// internal storage. Fields on *Job must only be mutated through Queue
// methods that hold the write lock; direct mutation by callers is a
// data race.
//
// # Persistence
//
// Save writes queue.json.gz (the index) plus one jobs/<id>.json.gz per
// job, each via the same atomic temp+fsync+rename pattern used by the
// config package. Load reverses the process. The on-disk format is
// versioned (see persistenceVersion) and intentionally readable with
// `zcat … | jq`.
package queue

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// Job is a live download job. It starts life when NewJob parses an NZB
// and ends when the downloader+postproc pipeline moves it to history.
//
// Fields are exported so encoding/json can marshal the full structure;
// callers must NOT mutate them outside the queue's lock.
type Job struct {
	// ID is a 16-character lowercase hex string produced from 8 bytes
	// of crypto/rand output. Stable for the life of the job.
	ID string `json:"id"`

	// Filename is the original NZB filename as supplied to Add. May be
	// empty when the caller had no filename (e.g. URL-grabbed NZBs
	// before the server provided a Content-Disposition).
	Filename string `json:"filename"`

	// Name is the display name. Defaults to Filename minus extension;
	// callers can override via AddOptions.Name.
	Name string `json:"name"`

	// Password is the archive password extracted from the filename or
	// supplied by the user. Empty if the job is unencrypted.
	Password string `json:"password,omitempty"` //nolint:gosec // G117: NZB archive password, not a credential

	// URL is the origin URL for URL-grabbed NZBs; empty for uploaded
	// or watched-dir NZBs.
	URL string `json:"url,omitempty"`

	// Category is the configured category name this job belongs to.
	// Resolved against the config's Categories list at download time.
	Category string `json:"category,omitempty"`

	// Priority is the user-selected priority. Queue ordering is driven
	// by this field at Add time; see insertByPriority.
	Priority constants.Priority `json:"priority"`

	// Status is the current lifecycle state. The queue manages
	// transitions between Queued and Paused; other states are driven
	// by the downloader and post-proc pipeline.
	Status constants.Status `json:"status"`

	// PP is the post-proc level 0-3 (download / +unpack / +repair / +delete).
	PP int `json:"pp"`

	// Script is the name of an optional user post-proc script.
	Script string `json:"script,omitempty"`

	// Added is the wall-clock time when the job entered the queue.
	Added time.Time `json:"added"`

	// DownloadStarted is the wall-clock time when the first article
	// began downloading. Zero if the job hasn't started yet.
	DownloadStarted time.Time `json:"download_started"`

	// DownloadFinished is the wall-clock time when the download phase
	// completed (all articles received). Zero until the download finishes.
	// Used to calculate download speed excluding post-processing time.
	DownloadFinished time.Time `json:"download_finished"`

	// ServerStats tracks successfully downloaded bytes per server.
	// Map: ServerName -> Bytes.
	ServerStats map[string]int64 `json:"server_stats,omitempty"`

	// Meta carries <meta> tags parsed from the NZB, preserved as a
	// slice-per-key to match the Python parser's multi-value semantics.
	Meta map[string][]string `json:"meta,omitempty"`

	// Groups is the de-duplicated union of newsgroups across files.
	Groups []string `json:"groups,omitempty"`

	// MD5 is the hex-encoded MD5 digest of article IDs. Used for
	// duplicate-job detection against history (Tranche B / Step 1.3).
	MD5 string `json:"md5"`

	// AvgAge is the mean posting date across the job's files, used
	// to sort the queue by "oldest first" and to trigger propagation
	// delay (downloads held back until articles have had time to
	// propagate across Usenet peers).
	AvgAge time.Time `json:"avg_age"`

	// Files holds the job's files in NZB source order.
	Files []JobFile `json:"files"`

	// TotalBytes is the byte count the NZB claimed — sum of
	// Files[].Bytes at Add time. Untrusted (poster-supplied) but
	// useful for UI and free-space pre-checks.
	TotalBytes int64 `json:"total_bytes"`

	// RemainingBytes is TotalBytes minus the sum of successfully completed
	// articles. Decremented as articles download successfully.
	RemainingBytes int64 `json:"remaining_bytes"`

	// FailedBytes is the sum of articles that failed all retries.
	// Used by the early health gate to abort hopeless jobs.
	FailedBytes int64 `json:"failed_bytes"`

	// Par2Bytes is the sum of all articles belonging to par2 files.
	// Used by the early health gate to determine the maximum repair capacity.
	Par2Bytes int64 `json:"par2_bytes"`

	// Par2Files is the count of par2 files in the NZB.
	Par2Files int `json:"par2_files"`

	// PostProc is set to true when the job is handed off to the
	// post-processor to prevent double-enqueuing.
	PostProc bool `json:"post_proc,omitempty"`

	// Warning holds a human-readable warning message (e.g. "Duplicate NZB").
	// Usually accompanied by StatusPaused.
	Warning string `json:"warning,omitempty"`

	// PendingArticles is the count of articles across all files that are
	// not Done and not Emitted. Maintained by queue mutation methods;
	// recomputed on load. Allows ForEachUnfinishedArticle to skip entire
	// jobs in O(1) when all articles are complete.
	PendingArticles int `json:"-"`

	// ArticlesResolved counts articles that have completed (success or
	// failure) since the job started. Used by the early-abort check.
	ArticlesResolved int `json:"-"`

	// ArticlesFailed counts articles that permanently failed since the
	// job started. Used by the early-abort check.
	ArticlesFailed int `json:"-"`

	// EarlyAborted is set to true when the early-abort check fires,
	// preventing duplicate abort callbacks.
	EarlyAborted bool `json:"-"`

	// artIdx is a transient, in-memory index from messageID → *JobArticle
	// for O(1) lookups. Built lazily by articleByID and not serialised
	// (article slice addresses change on deserialisation anyway).
	artIdx map[string]*JobArticle `json:"-"`
}

// articleByID returns the article with the given messageID, or nil if
// not found. On first call it lazily builds an index over all articles
// in the job so subsequent lookups are O(1) instead of O(files×articles).
// Must be called under the queue's lock (read or write).
func (j *Job) articleByID(messageID string) *JobArticle {
	if j.artIdx == nil {
		j.buildArtIndex()
	}
	return j.artIdx[messageID]
}

// buildArtIndex populates the messageID→*JobArticle index and sets
// FileIdx on each article for back-reference to the containing file.
func (j *Job) buildArtIndex() {
	j.artIdx = make(map[string]*JobArticle)
	for fi := range j.Files {
		for ai := range j.Files[fi].Articles {
			art := &j.Files[fi].Articles[ai]
			art.FileIdx = fi
			j.artIdx[art.ID] = art
		}
	}
}

const (
	// earlyAbortSample is the number of articles that must resolve
	// before the early-abort heuristic fires. Too small → false
	// positives on slow starts; too large → wasted bandwidth.
	earlyAbortSample = 10

	// earlyAbortThreshold is the failure rate (0.0–1.0) above which
	// the job is considered DMCA'd or expired. 80% matches SABnzbd's
	// heuristic for fully-removed NZBs.
	earlyAbortThreshold = 0.80
)

// IsEarlyAbort returns true if the job should be aborted based on the
// first-article failure rate. It checks after earlyAbortSample articles
// have resolved: if 80%+ failed, the download is likely DMCA'd or
// expired and further downloading wastes bandwidth.
//
// Must be called under the queue's lock (read or write).
func (j *Job) IsEarlyAbort() bool {
	if j.EarlyAborted {
		return false // already fired
	}
	if j.ArticlesResolved < earlyAbortSample {
		return false // not enough data yet
	}
	rate := float64(j.ArticlesFailed) / float64(j.ArticlesResolved)
	if rate >= earlyAbortThreshold {
		j.EarlyAborted = true
		return true
	}
	return false
}

// recomputePending recalculates Pending on every file and
// PendingArticles on the job from the ground truth (article Done/Emitted
// flags). Called on Load and ClearAllEmitted where batch state changes
// make incremental tracking impractical. Also builds the artIdx if not
// yet populated.
func (j *Job) recomputePending() {
	total := 0
	for fi := range j.Files {
		n := 0
		var downloaded int64
		for ai := range j.Files[fi].Articles {
			art := &j.Files[fi].Articles[ai]
			art.FileIdx = fi
			if !art.Done && !art.Emitted {
				n++
			}
			if art.Done && !art.Failed {
				downloaded += int64(art.Bytes)
			}
		}
		j.Files[fi].Pending = n
		j.Files[fi].BytesDownloaded = downloaded
		total += n
	}
	j.PendingArticles = total
}

// JobFile is a single file within a job: its articles, its assembly
// state, and the metadata needed to write it out.
type JobFile struct {
	Subject  string       `json:"subject"`
	Date     time.Time    `json:"date"`
	Articles []JobArticle `json:"articles"`
	Bytes    int64        `json:"bytes"`
	// Complete is set once all articles have downloaded and the file
	// has been assembled on disk.
	Complete bool `json:"complete,omitempty"`
	// AssembledCRC32 is the CRC32 computed by the assembler over the
	// fully-written file, derived by combining per-article yEnc CRCs
	// in offset order. Used by the QuickCheck post-processing stage
	// to verify file integrity against par2 file hashes without
	// re-reading the file from disk. Zero if unavailable (e.g. the
	// file contained UU-encoded or failed articles).
	AssembledCRC32 uint32 `json:"assembled_crc32,omitempty"`
	// Pending is the count of articles in this file that are not Done
	// and not Emitted. Maintained by queue mutation methods; recomputed
	// on load. Allows ForEachUnfinishedArticle to skip completed files
	// in O(1). Excluded from JSON since it's derived state.
	Pending int `json:"-"`
	// BytesDownloaded is the sum of byte counts for articles that have
	// completed successfully (Done && !Failed). Maintained incrementally
	// by mutation methods and recomputed by recomputePending on load.
	// Drives per-file progress in the UI's queue-row drawer.
	BytesDownloaded int64 `json:"-"`
}

// JobArticle is a single NNTP article. The structural fields (ID,
// Bytes, Number) are fixed at job-creation time; Done flips true when
// the downloader successfully fetches and decodes the article.
type JobArticle struct {
	ID     string `json:"id"`
	Bytes  int    `json:"bytes"`
	Number int    `json:"number"`
	Done   bool   `json:"done,omitempty"`
	// Failed is set to true if the article failed on all servers.
	Failed bool `json:"failed,omitempty"`
	// Emitted is a transient, in-memory flag: the downloader has handed
	// a result (success or permanent failure) to the pipeline, but the
	// assembler has not yet made the outcome durable (Done/Failed).
	// Excluded from JSON persistence so a restart re-dispatches articles
	// whose bytes hadn't reached stable storage before the crash.
	Emitted bool `json:"-"`
	// FileIdx is the index of the containing JobFile within Job.Files.
	// Set by buildArtIndex/recomputePending; used by mutation methods
	// to update the per-file Pending counter in O(1) without scanning
	// for the parent file.
	FileIdx int `json:"-"`
}

// AddOptions carries the call-site arguments for NewJob. Zero values
// are valid and produce sensible defaults.
type AddOptions struct {
	// Filename is the original NZB filename (may include a path).
	Filename string

	// Name overrides the display name. Empty means "derive from
	// Filename by stripping extensions".
	Name string

	Password string
	URL      string
	Category string

	// PP is the post-processing level. types.PPInherit (-1) means
	// "inherit from the job's category config".
	PP int

	// Script is the post-processing script name. Empty means
	// "inherit from the job's category config".
	Script string

	// Priority defaults to the category's priority when set to
	// constants.DefaultPriority. Zero means PriorityNormal.
	Priority constants.Priority

	// Categories is the full category config list, used to resolve
	// sentinel values for PP, Script, and Priority. May be nil in
	// tests; sentinels are left as-is when nil.
	Categories []config.CategoryConfig

	// Logger, if non-nil, receives structured log output for PP/script/
	// priority resolution decisions. Useful for diagnosing "why did this
	// job get PP=0?" questions.
	Logger *slog.Logger
}

// NewJob converts parser output plus caller options into a runtime
// Job ready to hand to Queue.Add. It allocates a fresh random ID and
// copies the parsed file/article structure into mutable runtime form.
//
// Returns an error only if the OS entropy source fails — treat that
// as fatal; the daemon has no safe fallback.
func NewJob(parsed *nzb.NZB, opts AddOptions, sOpts fsutil.SanitizeOptions) (*Job, error) {
	id, err := newJobID()
	if err != nil {
		return nil, err
	}

	name := opts.Name
	if name == "" {
		name = deriveName(opts.Filename)
	}

	// 1. Apply regex-based cleanup (strip spam prefixes/suffixes)
	name = fsutil.CleanupName(name, sOpts.CleanupList)

	// 2. Apply filesystem sanitization rules.
	name = fsutil.SanitizeFolderName(name, sOpts)

	// Resolve sentinel values from the matching category config.
	pp := opts.PP
	script := opts.Script
	priority := opts.Priority
	ppReason := "explicit"
	if opts.Categories != nil {
		cat := config.FindCategory(opts.Categories, opts.Category)
		if pp == types.PPInherit {
			pp = cat.PP
			ppReason = fmt.Sprintf("category %q", cat.Name)
		}
		if script == "" {
			script = cat.Script
		}
		if priority == constants.DefaultPriority {
			priority = constants.Priority(cat.Priority)
		}
	}
	// Clamp remaining sentinels to safe defaults when no categories
	// were provided (e.g. tests, CLI one-shot mode).
	if pp == types.PPInherit {
		pp = 0
		ppReason = "default (no categories configured)"
	}
	if priority == constants.DefaultPriority {
		priority = constants.NormalPriority
	}

	if opts.Logger != nil {
		opts.Logger.Info("job PP resolved",
			"job_name", name,
			"category", opts.Category,
			"requested_pp", opts.PP,
			"resolved_pp", pp,
			"reason", ppReason,
			"script", script,
			"priority", priority.String(),
		)
	}

	job := &Job{
		ID:       id,
		Filename: opts.Filename,
		Name:     name,
		Password: opts.Password,
		URL:      opts.URL,
		Category: opts.Category,
		Priority: priority,
		Status:   constants.StatusQueued,
		PP:       pp,
		Script:   script,
		Added:    time.Now().UTC(),
		Meta:     parsed.Meta,
		Groups:   parsed.Groups,
		MD5:      hex.EncodeToString(parsed.MD5[:]),
		AvgAge:   parsed.AvgAge,
	}

	job.Files = make([]JobFile, 0, len(parsed.Files))
	for _, pf := range parsed.Files {
		isPar2 := strings.Contains(strings.ToLower(pf.Subject), ".par2")
		jf := JobFile{
			Subject:  pf.Subject,
			Date:     pf.Date,
			Bytes:    pf.Bytes,
			Articles: make([]JobArticle, 0, len(pf.Articles)),
		}
		for _, pa := range pf.Articles {
			jf.Articles = append(jf.Articles, JobArticle{
				ID:     pa.ID,
				Bytes:  pa.Bytes,
				Number: pa.Number,
			})
		}
		job.Files = append(job.Files, jf)
		job.TotalBytes += pf.Bytes
		if isPar2 {
			job.Par2Bytes += pf.Bytes
			job.Par2Files++
		}
	}
	job.RemainingBytes = job.TotalBytes
	return job, nil
}

// IsComplete returns true if all files in the job are marked complete.
func (j *Job) IsComplete() bool {
	for i := range j.Files {
		if !j.Files[i].Complete {
			return false
		}
	}
	return true
}

// deriveName strips directory components and the extension from path.
// For "/watch/My.Show.S01E02.nzb" returns "My.Show.S01E02". A ".nzb.gz"
// or ".nzb.bz2" double extension is collapsed to the bare stem too.
func deriveName(path string) string {
	base := filepath.Base(path)
	// Strip compressed-NZB compound extensions first so "x.nzb.gz"
	// yields "x" rather than "x.nzb".
	for _, suffix := range []string{".nzb.gz", ".nzb.bz2"} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			return base[:len(base)-len(suffix)]
		}
	}
	if ext := filepath.Ext(base); ext != "" {
		return base[:len(base)-len(ext)]
	}
	return base
}

// newJobID returns a 16-character lowercase hex string backed by 8
// bytes (64 bits) of OS entropy. Collisions are vanishingly unlikely
// within any realistic job population; the queue still rejects Add
// when an ID collides.
func newJobID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
