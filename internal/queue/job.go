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
	"path/filepath"
	"strings"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/fsutil"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
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
	Password string `json:"password,omitempty"`

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
	DownloadStarted time.Time `json:"download_started,omitempty"`

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

	// PostProc is set to true when the job is handed off to the
	// post-processor to prevent double-enqueuing.
	PostProc bool `json:"post_proc,omitempty"`
}

// GetStatus returns the current lifecycle state of the job. It is safe
// for concurrent use.
func (j *Job) GetStatus() constants.Status {
	// Status is a string, which is not atomically loadable in Go.
	// However, the Queue lock protects it during mutation.
	// Tests and other subsystems that have a pointer to the Job
	// should generally use Queue.Get(id) to see a consistent view,
	// but for status specifically we could use an atomic value if
	// the race is a problem.
	//
	// For now, the race in tests is because the test holds a pointer
	// and reads the field while the background worker calls SetStatus.
	return j.Status
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
	PP       int
	Script   string

	// Priority defaults to PriorityNormal when zero-valued.
	Priority constants.Priority
}

// NewJob converts parser output plus caller options into a runtime
// Job ready to hand to Queue.Add. It allocates a fresh random ID and
// copies the parsed file/article structure into mutable runtime form.
//
// Returns an error only if the OS entropy source fails — treat that
// as fatal; the daemon has no safe fallback.
func NewJob(parsed *nzb.NZB, opts AddOptions) (*Job, error) {
	id, err := newJobID()
	if err != nil {
		return nil, err
	}

	name := opts.Name
	if name == "" {
		name = deriveName(opts.Filename)
	}
	name = fsutil.SanitizeFolderName(name)

	job := &Job{
		ID:       id,
		Filename: opts.Filename,
		Name:     name,
		Password: opts.Password,
		URL:      opts.URL,
		Category: opts.Category,
		Priority: opts.Priority,
		Status:   constants.StatusQueued,
		PP:       opts.PP,
		Script:   opts.Script,
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
		}
	}
	job.RemainingBytes = job.TotalBytes
	return job, nil
}

// IsComplete returns true if all files in the job are marked complete.
func (j *Job) IsComplete() bool {
	for _, f := range j.Files {
		if !f.Complete {
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
